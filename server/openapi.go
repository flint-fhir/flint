package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/flint-fhir/flint/pkg/fhirutil"
	"github.com/flint-fhir/flint/pkg/validation"
	"github.com/flint-fhir/flint/store/postgres"
)

// ConformanceMatrixEntry describes a verified capability in the Flint FHIR R4 Conformance Matrix.
type ConformanceMatrixEntry struct {
	Category      string `json:"category"`
	Capability    string `json:"capability"`
	Specification string `json:"specification"`
	Status        string `json:"status"`
	Endpoint      string `json:"endpoint"`
}

// ConformanceMatrix returns the canonical FHIR R4 / US Core / ONC (g)(10) conformance matrix for Flint.
func ConformanceMatrix() []ConformanceMatrixEntry {
	return []ConformanceMatrixEntry{
		{
			Category:      "Core Storage",
			Capability:    "Proto3 Wire Binary Store (Zero JSONB) — All 146 FHIR R4 Resource Types",
			Specification: "Google FHIR R4 Protobuf + 6 HAPI spidx_* Covering Tables",
			Status:        "Verified",
			Endpoint:      "/fhir/r4/{tenant}/{resourceType}",
		},
		{
			Category:      "FHIR R4 CRUD, Versioning & Patch",
			Capability:    "read, vread, create, update, patch (RFC 6902 JSON Patch, RFC 7396 Merge Patch & HL7 FHIRPath Parameters Patch), delete, instance/type/system _history, ETag / If-Match CAS",
			Specification: "HL7 FHIR R4 RESTful API (4.0.1), FHIRPath Patch & RFC 6902 / RFC 7396",
			Status:        "Verified",
			Endpoint:      "/fhir/r4/{tenant}/{resourceType}/{id}",
		},
		{
			Category:      "Conditional Operations & Bundles",
			Capability:    "Conditional Read (If-None-Match / If-Modified-Since -> 304 Not Modified & HEAD), Conditional Create (If-None-Exist), Conditional Update, Conditional Patch, Conditional Delete (single & _count multiple), Intra-Bundle Conditional References, Prefer Header (return=minimal|representation|OperationOutcome, handling=strict|lenient, respond-async) & Atomic ACID Transaction / Batch Bundles (DELETE -> POST -> PUT/PATCH -> GET/HEAD)",
			Specification: "HL7 FHIR R4 Bundle & Conditional Operations",
			Status:        "Verified",
			Endpoint:      "/fhir/r4/{tenant}/{resourceType}?identifier=...",
		},
		{
			Category:      "Advanced Search Engine",
			Capability:    "Date/Quantity Ranges (eq, ne, gt, lt, ge, le, sa, eb, ap), Composite Search (code-value-quantity, component-code-value-quantity, combo-code-value-quantity, code-value-concept, component-code-value-concept, combo-code-value-concept, code-value-date, code-value-string), Token :of-type & Terminology-Aware Modifiers (:in, :not-in, :below, :above), Reference :identifier Modifier (spidx_reference + inline Reference.identifier), Multi-Hop Chained Search (:exact/:contains/:not, reference & URI :below/:above), Multi-Hop Recursive _has Reverse Chaining, _list Cohort Search, _profile / _tag / _security / _source Meta Search, Wildcard & Iterative _include / _revinclude (*, :iterate), Choice-Type :missing, Multi-field _sort, _summary / _elements (SUBSETTED), _total=none, Bundle.link Pagination, POST /_search",
			Specification: "HL7 FHIR R4 Search",
			Status:        "Verified",
			Endpoint:      "/fhir/r4/{tenant}/{resourceType}?_include=...",
		},
		{
			Category:      "Patient Compartment, IPS, SDC & Da Vinci Operations",
			Capability:    "Patient/$everything & Encounter/$everything (GET/POST + _type/_since/_typeFilter/_count/_offset pagination), Patient/{id}/$summary (HL7 IPS), DocumentReference/$docref (US Core / Argonaut Clinical Notes + On-Demand IPS), Composition/{id}/$document (2-hop closure + persist=true), Questionnaire/$populate & QuestionnaireResponse/$extract (HL7 SDC + Auto-Provenance), Measure/$evaluate-measure (individual, summary, subject-list) & Measure/$care-gaps (Da Vinci DEQM), X-Provenance Header & /{resourceType}/{id}/$provenance, Patient/$match (MPI), Patient/$member-match (Da Vinci HRex), Claim/$submit (Da Vinci PAS), System/Type/Instance $meta / $meta-add / $meta-delete & RESTful Compartment Search",
			Specification: "HL7 FHIR R4, US Core, IPS, SDC & Da Vinci HRex/PDex/PAS/DEQM",
			Status:        "Verified",
			Endpoint:      "/fhir/r4/{tenant}/DocumentReference/$docref",
		},
		{
			Category:      "Terminology Services",
			Capability:    "ValueSet/$expand (with compose.include.filter, valueSet imports & exclude), ValueSet/$validate-code, CodeSystem/$lookup, CodeSystem/$validate-code, CodeSystem/$subsumes & ConceptMap/$translate",
			Specification: "HL7 FHIR R4 Terminology Services (4.0.1)",
			Status:        "Verified",
			Endpoint:      "/fhir/r4/{tenant}/ValueSet/$expand",
		},
		{
			Category:      "FHIR R4 Subscriptions",
			Capability:    "Subscription Lifecycle (requested -> active) & Automated rest-hook Webhook Delivery",
			Specification: "HL7 FHIR R4 Subscription (rest-hook)",
			Status:        "Verified",
			Endpoint:      "/fhir/r4/{tenant}/Subscription",
		},
		{
			Category:      "Bulk Data Access",
			Capability:    "System & Patient $export (NDJSON Stream + Postgres-Persisted Async Polling)",
			Specification: "HL7 FHIR Bulk Data Access IG STU2 / ONC (g)(10)",
			Status:        "Verified",
			Endpoint:      "/fhir/r4/{tenant}/$export",
		},
		{
			Category:      "Conformance, Validation & Certification",
			Capability:    "StructureDefinition & ValueSet Validation ($validate, 26 US Core v6.1.0 & v7.0.0 Profiles) & 50-Check Automated Certification ($certify)",
			Specification: "HL7 FHIR R4 OperationDefinition/Resource-validate & TestReport",
			Status:        "Verified",
			Endpoint:      "/fhir/r4/{tenant}/$certify",
		},
		{
			Category:      "Security & Authorization",
			Capability:    "Built-in OAuth2 Server (PKCE S256, Backend Services private_key_jwt RFC 7523, Postgres Refresh/Revocation), Granular SMART v2 Scopes, OIDC/Zitadel JWT & Patient Compartment Isolation",
			Specification: "SMART App Launch 2.0.0 & Backend Services IG",
			Status:        "Verified",
			Endpoint:      "/fhir/r4/{tenant}/.well-known/smart-configuration",
		},
		{
			Category:      "Security Audit Logging",
			Capability:    "Immutable HIPAA / ONC AuditEvent Recording & Search",
			Specification: "HL7 FHIR R4 AuditEvent / US Core",
			Status:        "Verified",
			Endpoint:      "/fhir/r4/{tenant}/AuditEvent",
		},
		{
			Category:      "Analytical Lakehouse",
			Capability:    "AutoMQ CDC -> Iceberg Table Topics -> SQLMesh Patient 360, CMS165 / CMS122 eCQM & Claims Utilization Marts",
			Specification: "Apache Iceberg + SQLMesh (DuckDB)",
			Status:        "Verified",
			Endpoint:      "analytics/models/mart_patient_360.sql",
		},
	}
}

func resourceSearchParamDefs() map[string][]map[string]string {
	defs := map[string][]map[string]string{
		"Patient": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "family", "type": "string"},
			{"name": "given", "type": "string"},
			{"name": "name", "type": "string"},
			{"name": "identifier", "type": "token"},
			{"name": "gender", "type": "token"},
			{"name": "birthdate", "type": "date"},
			{"name": "organization", "type": "reference"},
		},
		"Condition": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "code", "type": "token"},
			{"name": "clinical-status", "type": "token"},
			{"name": "verification-status", "type": "token"},
			{"name": "category", "type": "token"},
			{"name": "patient", "type": "reference"},
			{"name": "subject", "type": "reference"},
			{"name": "encounter", "type": "reference"},
			{"name": "onset-date", "type": "date"},
		},
		"Encounter": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "class", "type": "token"},
			{"name": "status", "type": "token"},
			{"name": "type", "type": "token"},
			{"name": "patient", "type": "reference"},
			{"name": "subject", "type": "reference"},
			{"name": "date", "type": "date"},
			{"name": "participant", "type": "reference"},
			{"name": "location", "type": "reference"},
		},
		"Observation": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "code", "type": "token"},
			{"name": "component-code", "type": "token"},
			{"name": "combo-code", "type": "token"},
			{"name": "status", "type": "token"},
			{"name": "category", "type": "token"},
			{"name": "patient", "type": "reference"},
			{"name": "subject", "type": "reference"},
			{"name": "date", "type": "date"},
			{"name": "performer", "type": "reference"},
			{"name": "value-quantity", "type": "quantity"},
			{"name": "component-value-quantity", "type": "quantity"},
			{"name": "combo-value-quantity", "type": "quantity"},
			{"name": "value-concept", "type": "token"},
			{"name": "component-value-concept", "type": "token"},
			{"name": "combo-value-concept", "type": "token"},
			{"name": "value-date", "type": "date"},
			{"name": "value-string", "type": "string"},
			{"name": "code-value-quantity", "type": "composite"},
			{"name": "component-code-value-quantity", "type": "composite"},
			{"name": "combo-code-value-quantity", "type": "composite"},
			{"name": "code-value-concept", "type": "composite"},
			{"name": "component-code-value-concept", "type": "composite"},
			{"name": "combo-code-value-concept", "type": "composite"},
			{"name": "code-value-date", "type": "composite"},
			{"name": "code-value-string", "type": "composite"},
		},
		"Practitioner": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "family", "type": "string"},
			{"name": "given", "type": "string"},
			{"name": "name", "type": "string"},
			{"name": "identifier", "type": "token"},
			{"name": "active", "type": "token"},
			{"name": "gender", "type": "token"},
			{"name": "email", "type": "token"},
			{"name": "phone", "type": "token"},
			{"name": "address", "type": "string"},
		},
		"MedicationRequest": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "code", "type": "token"},
			{"name": "status", "type": "token"},
			{"name": "intent", "type": "token"},
			{"name": "category", "type": "token"},
			{"name": "patient", "type": "reference"},
			{"name": "subject", "type": "reference"},
			{"name": "encounter", "type": "reference"},
			{"name": "requester", "type": "reference"},
			{"name": "authoredon", "type": "date"},
		},
		"AllergyIntolerance": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "code", "type": "token"},
			{"name": "clinical-status", "type": "token"},
			{"name": "verification-status", "type": "token"},
			{"name": "category", "type": "token"},
			{"name": "criticality", "type": "token"},
			{"name": "patient", "type": "reference"},
			{"name": "subject", "type": "reference"},
			{"name": "date", "type": "date"},
		},
		"Procedure": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "code", "type": "token"},
			{"name": "status", "type": "token"},
			{"name": "category", "type": "token"},
			{"name": "patient", "type": "reference"},
			{"name": "subject", "type": "reference"},
			{"name": "encounter", "type": "reference"},
			{"name": "performer", "type": "reference"},
			{"name": "date", "type": "date"},
		},
		"DiagnosticReport": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "code", "type": "token"},
			{"name": "status", "type": "token"},
			{"name": "category", "type": "token"},
			{"name": "patient", "type": "reference"},
			{"name": "subject", "type": "reference"},
			{"name": "encounter", "type": "reference"},
			{"name": "result", "type": "reference"},
			{"name": "specimen", "type": "reference"},
			{"name": "date", "type": "date"},
			{"name": "issued", "type": "date"},
		},
		"Immunization": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "vaccine-code", "type": "token"},
			{"name": "status", "type": "token"},
			{"name": "patient", "type": "reference"},
			{"name": "subject", "type": "reference"},
			{"name": "date", "type": "date"},
			{"name": "lot-number", "type": "string"},
		},
		"Organization": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "name", "type": "string"},
			{"name": "identifier", "type": "token"},
			{"name": "active", "type": "token"},
			{"name": "type", "type": "token"},
			{"name": "address", "type": "string"},
			{"name": "address-city", "type": "string"},
			{"name": "address-state", "type": "string"},
			{"name": "address-postalcode", "type": "string"},
			{"name": "partof", "type": "reference"},
		},
		"Specimen": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "identifier", "type": "token"},
			{"name": "accession", "type": "token"},
			{"name": "status", "type": "token"},
			{"name": "type", "type": "token"},
			{"name": "patient", "type": "reference"},
			{"name": "subject", "type": "reference"},
			{"name": "collected", "type": "date"},
			{"name": "bodysite", "type": "token"},
			{"name": "collector", "type": "reference"},
		},
		"Location": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "name", "type": "string"},
			{"name": "identifier", "type": "token"},
			{"name": "status", "type": "token"},
			{"name": "type", "type": "token"},
			{"name": "address", "type": "string"},
			{"name": "address-city", "type": "string"},
			{"name": "address-state", "type": "string"},
			{"name": "address-postalcode", "type": "string"},
			{"name": "organization", "type": "reference"},
			{"name": "partof", "type": "reference"},
		},
		"DocumentReference": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "identifier", "type": "token"},
			{"name": "status", "type": "token"},
			{"name": "type", "type": "token"},
			{"name": "category", "type": "token"},
			{"name": "patient", "type": "reference"},
			{"name": "subject", "type": "reference"},
			{"name": "date", "type": "date"},
			{"name": "period", "type": "date"},
			{"name": "author", "type": "reference"},
			{"name": "custodian", "type": "reference"},
		},
		"Provenance": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "target", "type": "reference"},
			{"name": "patient", "type": "reference"},
			{"name": "recorded", "type": "date"},
			{"name": "agent", "type": "reference"},
		},
		"CarePlan": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "patient", "type": "reference"},
			{"name": "subject", "type": "reference"},
			{"name": "status", "type": "token"},
			{"name": "intent", "type": "token"},
			{"name": "category", "type": "token"},
			{"name": "date", "type": "date"},
		},
		"CareTeam": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "patient", "type": "reference"},
			{"name": "subject", "type": "reference"},
			{"name": "status", "type": "token"},
			{"name": "role", "type": "token"},
			{"name": "participant", "type": "reference"},
		},
		"Goal": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "patient", "type": "reference"},
			{"name": "subject", "type": "reference"},
			{"name": "lifecycle-status", "type": "token"},
			{"name": "target-date", "type": "date"},
			{"name": "description", "type": "token"},
		},
		"ServiceRequest": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "patient", "type": "reference"},
			{"name": "subject", "type": "reference"},
			{"name": "status", "type": "token"},
			{"name": "intent", "type": "token"},
			{"name": "category", "type": "token"},
			{"name": "code", "type": "token"},
			{"name": "authored", "type": "date"},
			{"name": "requester", "type": "reference"},
			{"name": "performer", "type": "reference"},
		},
		"Coverage": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "patient", "type": "reference"},
			{"name": "beneficiary", "type": "reference"},
			{"name": "payor", "type": "reference"},
			{"name": "status", "type": "token"},
			{"name": "type", "type": "token"},
			{"name": "identifier", "type": "token"},
			{"name": "subscriber-id", "type": "token"},
		},
		"QuestionnaireResponse": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "patient", "type": "reference"},
			{"name": "subject", "type": "reference"},
			{"name": "status", "type": "token"},
			{"name": "authored", "type": "date"},
			{"name": "questionnaire", "type": "token"},
		},
		"MedicationDispense": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "patient", "type": "reference"},
			{"name": "subject", "type": "reference"},
			{"name": "status", "type": "token"},
			{"name": "type", "type": "token"},
			{"name": "code", "type": "token"},
			{"name": "medication", "type": "reference"},
		},
		"Medication": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "code", "type": "token"},
			{"name": "status", "type": "token"},
			{"name": "identifier", "type": "token"},
		},
		"Device": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "patient", "type": "reference"},
			{"name": "type", "type": "token"},
			{"name": "status", "type": "token"},
			{"name": "identifier", "type": "token"},
			{"name": "udi-carrier", "type": "string"},
		},
		"PractitionerRole": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "practitioner", "type": "reference"},
			{"name": "organization", "type": "reference"},
			{"name": "location", "type": "reference"},
			{"name": "specialty", "type": "token"},
			{"name": "role", "type": "token"},
			{"name": "identifier", "type": "token"},
		},
		"RelatedPerson": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "patient", "type": "reference"},
			{"name": "name", "type": "string"},
			{"name": "relationship", "type": "token"},
			{"name": "identifier", "type": "token"},
		},
		"Subscription": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "status", "type": "token"},
			{"name": "type", "type": "token"},
			{"name": "criteria", "type": "string"},
			{"name": "url", "type": "uri"},
			{"name": "payload", "type": "token"},
		},
		"ValueSet": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "url", "type": "uri"},
			{"name": "name", "type": "string"},
			{"name": "status", "type": "token"},
			{"name": "version", "type": "token"},
		},
		"CodeSystem": {
			{"name": "_id", "type": "token"},
			{"name": "_lastUpdated", "type": "date"},
			{"name": "url", "type": "uri"},
			{"name": "name", "type": "string"},
			{"name": "status", "type": "token"},
			{"name": "version", "type": "token"},
		},
	}
	metaParams := []map[string]string{
		{"name": "_profile", "type": "uri"},
		{"name": "_tag", "type": "token"},
		{"name": "_security", "type": "token"},
		{"name": "_source", "type": "uri"},
	}
	for rt, sp := range defs {
		defs[rt] = append(sp, metaParams...)
	}
	return defs
}

// BuildOpenAPISpec dynamically synthesizes an OpenAPI 3.1.0 specification for a Flint tenant
// directly from the registered google/fhir Protobuf MessageDescriptors (protoreflect),
// StructureDefinition required field rules, search parameters, and SMART security config.
func (s *Server) BuildOpenAPISpec(tenant string) map[string]any {
	if strings.TrimSpace(tenant) == "" {
		tenant = "demo"
	}
	baseURL := "/fhir/r4/" + tenant

	schemas := make(map[string]any)
	defaultSchemas := validation.DefaultSchemas()
	resOrder := fhirutil.USCoreResourceTypes()
	seenRes := make(map[string]bool, len(resOrder))
	for _, rt := range resOrder {
		seenRes[rt] = true
	}
	var extraRes []string
	for rt := range s.protoRegistry {
		if !seenRes[rt] && rt != "AuditEvent" && rt != "Bundle" && rt != "OperationOutcome" {
			extraRes = append(extraRes, rt)
		}
	}
	sort.Strings(extraRes)
	resOrder = append(resOrder, extraRes...)

	for _, rt := range resOrder {
		sch := defaultSchemas[rt]
		md := s.protoRegistry[rt]
		if md != nil && seenRes[rt] {
			protoSchema := fhirutil.ProtoDescriptorToOpenAPISchema(rt, md, sch.RequiredFields)
			// Annotate ValueSet URI bindings from validation schema where present
			if props, ok := protoSchema["properties"].(map[string]any); ok {
				for field, vsURI := range sch.ValueSetURIs {
					if propMap, ok := props[field].(map[string]any); ok {
						propMap["x-fhir-valueset"] = vsURI
						if _, hasEnum := propMap["enum"]; !hasEnum {
							if binding, hasBinding := sch.CodeBindings[field]; hasBinding {
								codes := make([]string, 0, len(binding))
								for c := range binding {
									codes = append(codes, c)
								}
								sort.Strings(codes)
								propMap["x-fhir-bound-codes"] = codes
							}
						}
					}
				}
			}
			schemas[rt] = protoSchema
			continue
		}

		// Fallback when no proto descriptor is registered (e.g. lightweight unit test server)
		props := make(map[string]any)
		fieldNames := make([]string, 0, len(sch.AllowedFields))
		for f := range sch.AllowedFields {
			fieldNames = append(fieldNames, f)
		}
		sort.Strings(fieldNames)
		for _, f := range fieldNames {
			if f == "resourceType" {
				props[f] = map[string]any{"type": "string", "enum": []string{rt}}
				continue
			}
			propDef := map[string]any{"description": fmt.Sprintf("FHIR R4 %s.%s element", rt, f)}
			if binding, hasBinding := sch.CodeBindings[f]; hasBinding {
				codes := make([]string, 0, len(binding))
				for c := range binding {
					codes = append(codes, c)
				}
				sort.Strings(codes)
				propDef["type"] = "string"
				propDef["enum"] = codes
			}
			props[f] = propDef
		}
		schemas[rt] = map[string]any{
			"type":        "object",
			"description": fmt.Sprintf("HL7 FHIR R4 %s resource", rt),
			"required":    append([]string{"resourceType"}, sch.RequiredFields...),
			"properties":  props,
		}
	}

	schemas["OperationOutcome"] = map[string]any{
		"type":        "object",
		"description": "HL7 FHIR R4 OperationOutcome for structured validation and error reporting",
		"required":    []string{"resourceType", "issue"},
		"properties": map[string]any{
			"resourceType": map[string]any{"type": "string", "enum": []string{"OperationOutcome"}},
			"issue":        map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
		},
	}

	schemas["Bundle"] = map[string]any{
		"type":        "object",
		"description": "HL7 FHIR R4 Bundle (searchset, transaction, batch, history, document, collection)",
		"required":    []string{"resourceType", "type"},
		"properties": map[string]any{
			"resourceType": map[string]any{"type": "string", "enum": []string{"Bundle"}},
			"type":         map[string]any{"type": "string", "enum": []string{"transaction", "batch", "searchset", "history", "document", "collection"}},
			"total":        map[string]any{"type": "integer"},
			"link":         map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
			"entry":        map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
		},
	}

	schemas["Parameters"] = map[string]any{
		"type":        "object",
		"description": "HL7 FHIR R4 Parameters resource for terminology and operation inputs/outputs",
		"required":    []string{"resourceType"},
		"properties": map[string]any{
			"resourceType": map[string]any{"type": "string", "enum": []string{"Parameters"}},
			"parameter":    map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
		},
	}

	schemas["TestReport"] = map[string]any{
		"type":        "object",
		"description": "HL7 FHIR R4 TestReport generated by the 50-check automated certification suite ($certify)",
		"required":    []string{"resourceType", "status", "result"},
		"properties": map[string]any{
			"resourceType": map[string]any{"type": "string", "enum": []string{"TestReport"}},
			"status":       map[string]any{"type": "string", "enum": []string{"completed", "in-progress", "waiting", "stopped", "entered-in-error"}},
			"result":       map[string]any{"type": "string", "enum": []string{"pass", "fail", "pending"}},
			"score":        map[string]any{"type": "number"},
			"test":         map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
		},
	}

	schemas["BulkExportManifest"] = map[string]any{
		"type":        "object",
		"description": "HL7 FHIR Bulk Data Access IG STU2 Completion Manifest",
		"required":    []string{"transactionTime", "request", "requiresAccessToken", "output", "error"},
		"properties": map[string]any{
			"transactionTime":     map[string]any{"type": "string", "format": "date-time"},
			"request":             map[string]any{"type": "string"},
			"requiresAccessToken": map[string]any{"type": "boolean"},
			"output":              map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
			"error":               map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
		},
	}

	paths := map[string]any{
		"/healthz": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Observability & Operations"},
				"summary":     "Liveness probe (HTTP 200 OK)",
				"operationId": "getHealthz",
				"responses": map[string]any{
					"200": map[string]any{"description": "Server process is alive"},
				},
			},
		},
		"/readyz": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Observability & Operations"},
				"summary":     "Readiness probe verifying PostgreSQL operational store connectivity",
				"operationId": "getReadyz",
				"responses": map[string]any{
					"200": map[string]any{"description": "Server and PostgreSQL store are ready"},
					"503": map[string]any{"description": "Database connectivity check failed"},
				},
			},
		},
		"/metrics": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Observability & Operations"},
				"summary":     "Prometheus exposition format operational metrics",
				"operationId": "getPrometheusMetrics",
				"responses": map[string]any{
					"200": map[string]any{"description": "Prometheus text/plain 0.0.4 metrics"},
				},
			},
		},
		"/metadata": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Discovery & Docs"},
				"summary":     "Get FHIR R4 CapabilityStatement",
				"operationId": "getCapabilityStatement",
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR CapabilityStatement"},
				},
			},
		},
		"/.well-known/smart-configuration": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Discovery & Docs"},
				"summary":     "Get SMART on FHIR v2 Configuration",
				"operationId": "getSMARTConfiguration",
				"responses": map[string]any{
					"200": map[string]any{"description": "SMART 2.0 Discovery Document"},
				},
			},
		},
		"/.well-known/openid-configuration": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Discovery & Docs"},
				"summary":     "Get OpenID Connect 1.0 Discovery Configuration",
				"operationId": "getOIDCConfiguration",
				"responses": map[string]any{
					"200": map[string]any{"description": "OIDC Discovery Document"},
				},
			},
		},
		"/.well-known/jwks.json": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Discovery & Docs"},
				"summary":     "Get JSON Web Key Set (JWKS) for SMART JWT verification",
				"operationId": "getJWKS",
				"responses": map[string]any{
					"200": map[string]any{"description": "RFC 7517 JWKS Document"},
				},
			},
		},
		"/openapi.json": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Discovery & Docs"},
				"summary":     "Get OpenAPI 3.1.0 Specification",
				"operationId": "getOpenAPISpec",
				"responses": map[string]any{
					"200": map[string]any{"description": "OpenAPI 3.1.0 JSON Document"},
				},
			},
		},
		"/$docs": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Discovery & Docs"},
				"summary":     "Interactive OpenAPI 3.1 & Conformance Explorer UI",
				"operationId": "getDocsExplorer",
				"responses": map[string]any{
					"200": map[string]any{"description": "Interactive HTML Explorer"},
				},
			},
		},
		"/": map[string]any{
			"get": map[string]any{
				"tags":        []string{"System Operations"},
				"summary":     "System-level multi-type FHIR search across resource types (supports _type, _id, _lastUpdated, patient, code, _include, _revinclude)",
				"operationId": "systemSearchGet",
				"parameters": []map[string]any{
					{"name": "_type", "in": "query", "required": false, "description": "Comma-separated FHIR resource types (e.g. Patient,Observation,Condition)", "schema": map[string]any{"type": "string"}},
					{"name": "_id", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "_lastUpdated", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "patient", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "_count", "in": "query", "required": false, "schema": map[string]any{"type": "integer"}},
					{"name": "_offset", "in": "query", "required": false, "schema": map[string]any{"type": "integer"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle across requested FHIR resource types"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"System Operations"},
				"summary":     "Execute ACID Transaction or Batch Bundle",
				"operationId": "executeBundle",
				"responses": map[string]any{
					"200": map[string]any{"description": "Transaction or Batch response Bundle"},
					"400": map[string]any{"description": "Validation failure OperationOutcome"},
					"412": map[string]any{"description": "If-Match / If-None-Exist precondition failure"},
				},
			},
		},
		"/_search": map[string]any{
			"post": map[string]any{
				"tags":        []string{"System Operations"},
				"summary":     "System-level multi-type FHIR search via POST form body or query parameters (_type=Patient,Observation,Condition)",
				"operationId": "systemSearchPost",
				"parameters": []map[string]any{
					{"name": "_type", "in": "query", "required": false, "description": "Comma-separated FHIR resource types (e.g. Patient,Observation,Condition)", "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle across requested FHIR resource types"},
				},
			},
		},
		"/_history": map[string]any{
			"get": map[string]any{
				"tags":        []string{"System Operations"},
				"summary":     "System-level version history Bundle across all FHIR resource types (supports _since, _at, _list, _summary, _elements)",
				"operationId": "systemHistory",
				"parameters": []map[string]any{
					{"name": "_count", "in": "query", "schema": map[string]any{"type": "integer"}},
					{"name": "_offset", "in": "query", "schema": map[string]any{"type": "integer"}},
					{"name": "_since", "in": "query", "description": "ISO-8601 lower bound timestamp", "schema": map[string]any{"type": "string"}},
					{"name": "_at", "in": "query", "description": "Point-in-time or period currency filter (with optional comparator prefix eq, lt, le, gt, ge)", "schema": map[string]any{"type": "string"}},
					{"name": "_list", "in": "query", "description": "Cohort List or Group ID filter", "schema": map[string]any{"type": "string"}},
					{"name": "_summary", "in": "query", "description": "Summary projection mode (true, text, data, count, false)", "schema": map[string]any{"type": "string"}},
					{"name": "_elements", "in": "query", "description": "Comma-separated top-level elements to project", "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "History Bundle (type: history)"},
				},
			},
		},
		"/$validate": map[string]any{
			"post": map[string]any{
				"tags":        []string{"System Operations"},
				"summary":     "Validate any FHIR R4 resource against StructureDefinition & ValueSets",
				"operationId": "validateSystemResource",
				"responses": map[string]any{
					"200": map[string]any{"description": "OperationOutcome validation result"},
				},
			},
		},
		"/$meta": map[string]any{
			"get": map[string]any{
				"tags":        []string{"System Operations"},
				"summary":     "System-level summary of profiles, tags, and security labels across the server ($meta)",
				"operationId": "getSystemMeta",
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters (return.valueMeta)"},
				},
			},
		},
		"/$certify": map[string]any{
			"get": map[string]any{
				"tags":        []string{"System Operations"},
				"summary":     "Run 50-check automated FHIR R4, US Core v6.1.0 & v7.0.0, CQM, CDS Hooks, SDC, IPS, Da Vinci, and ONC (g)(10) certification suite",
				"operationId": "runCertificationSuiteGet",
				"responses": map[string]any{
					"200": map[string]any{"description": "HL7 FHIR R4 TestReport resource"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"System Operations"},
				"summary":     "Run 50-check automated FHIR R4, US Core v6.1.0 & v7.0.0, CQM, CDS Hooks, SDC, IPS, Da Vinci, and ONC (g)(10) certification suite",
				"operationId": "runCertificationSuitePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "HL7 FHIR R4 TestReport resource"},
				},
			},
		},
		"/Measure/$evaluate-measure": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Clinical Quality Measures (eCQM)"},
				"summary":     "Evaluate a Clinical Quality Measure (e.g. CMS165, CMS122) across a cohort or individual patient",
				"operationId": "evaluateMeasureGet",
				"parameters": []map[string]any{
					{"name": "measure", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "periodStart", "in": "query", "required": false, "schema": map[string]any{"type": "string", "format": "date"}},
					{"name": "periodEnd", "in": "query", "required": false, "schema": map[string]any{"type": "string", "format": "date"}},
					{"name": "subject", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "reportType", "in": "query", "required": false, "schema": map[string]any{"type": "string", "enum": []string{"individual", "summary", "subject-list"}}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "HL7 FHIR R4 MeasureReport resource"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Clinical Quality Measures (eCQM)"},
				"summary":     "Evaluate a Clinical Quality Measure via FHIR Parameters POST body",
				"operationId": "evaluateMeasurePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "HL7 FHIR R4 MeasureReport resource"},
				},
			},
		},
		"/Measure/{id}/$evaluate-measure": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Clinical Quality Measures (eCQM)"},
				"summary":     "Evaluate a Clinical Quality Measure by Measure ID (e.g. cms165-bp-control, cms122-diabetes-hba1c)",
				"operationId": "evaluateMeasureByIdGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "periodStart", "in": "query", "required": false, "schema": map[string]any{"type": "string", "format": "date"}},
					{"name": "periodEnd", "in": "query", "required": false, "schema": map[string]any{"type": "string", "format": "date"}},
					{"name": "subject", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "HL7 FHIR R4 MeasureReport resource"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Clinical Quality Measures (eCQM)"},
				"summary":     "Evaluate a Clinical Quality Measure by Measure ID via Parameters POST body",
				"operationId": "evaluateMeasureByIdPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "HL7 FHIR R4 MeasureReport resource"},
				},
			},
		},
		"/Measure/$care-gaps": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Clinical Quality Measures (eCQM)"},
				"summary":     "Generate HL7 Da Vinci DEQM Gaps-in-Care document Bundle ($care-gaps)",
				"operationId": "careGapsGet",
				"parameters": []map[string]any{
					{"name": "periodStart", "in": "query", "required": false, "schema": map[string]any{"type": "string", "format": "date"}},
					{"name": "periodEnd", "in": "query", "required": false, "schema": map[string]any{"type": "string", "format": "date"}},
					{"name": "subject", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "status", "in": "query", "required": false, "schema": map[string]any{"type": "string", "enum": []string{"open-gap", "closed-gap", "not-in-population"}}},
					{"name": "measureId", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "HL7 Da Vinci DEQM Gaps in Care Bundle (type: document)"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Clinical Quality Measures (eCQM)"},
				"summary":     "Generate HL7 Da Vinci DEQM Gaps-in-Care document Bundle via Parameters POST body",
				"operationId": "careGapsPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "HL7 Da Vinci DEQM Gaps in Care Bundle (type: document)"},
				},
			},
		},
		"/Measure/{id}/$care-gaps": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Clinical Quality Measures (eCQM)"},
				"summary":     "Generate HL7 Da Vinci DEQM Gaps-in-Care document Bundle for Measure/{id}",
				"operationId": "careGapsByIdGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "periodStart", "in": "query", "required": false, "schema": map[string]any{"type": "string", "format": "date"}},
					{"name": "periodEnd", "in": "query", "required": false, "schema": map[string]any{"type": "string", "format": "date"}},
					{"name": "subject", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "status", "in": "query", "required": false, "schema": map[string]any{"type": "string", "enum": []string{"open-gap", "closed-gap", "not-in-population"}}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "HL7 Da Vinci DEQM Gaps in Care Bundle (type: document)"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Clinical Quality Measures (eCQM)"},
				"summary":     "Generate HL7 Da Vinci DEQM Gaps-in-Care document Bundle for Measure/{id} (POST)",
				"operationId": "careGapsByIdPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "HL7 Da Vinci DEQM Gaps in Care Bundle (type: document)"},
				},
			},
		},
		"/cds-services": map[string]any{
			"get": map[string]any{
				"tags":        []string{"CDS Hooks v2.0"},
				"summary":     "Discover built-in HL7 CDS Hooks v2.0 Clinical Decision Support services",
				"operationId": "discoverCDSServices",
				"responses": map[string]any{
					"200": map[string]any{"description": "CDS Hooks v2.0 Service Discovery List"},
				},
			},
		},
		"/cds-services/{serviceId}": map[string]any{
			"post": map[string]any{
				"tags":        []string{"CDS Hooks v2.0"},
				"summary":     "Invoke an HL7 CDS Hooks v2.0 service (patient-view, order-select) with automatic prefetch hydration",
				"operationId": "invokeCDSService",
				"parameters": []map[string]any{
					{"name": "serviceId", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "CDS Hooks v2.0 Cards response"},
				},
			},
		},
		"/ValueSet/$expand": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Terminology Services"},
				"summary":     "Expand a FHIR R4 ValueSet by canonical URL and optional text filter",
				"operationId": "expandValueSetGet",
				"parameters": []map[string]any{
					{"name": "url", "in": "query", "required": false, "description": "Canonical ValueSet URL (e.g. http://hl7.org/fhir/ValueSet/administrative-gender)", "schema": map[string]any{"type": "string"}},
					{"name": "filter", "in": "query", "required": false, "description": "Case-insensitive code or display substring filter", "schema": map[string]any{"type": "string"}},
					{"name": "count", "in": "query", "required": false, "schema": map[string]any{"type": "integer"}},
					{"name": "offset", "in": "query", "required": false, "schema": map[string]any{"type": "integer"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Expanded FHIR R4 ValueSet resource"},
					"404": map[string]any{"description": "ValueSet not found"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Terminology Services"},
				"summary":     "Expand a FHIR R4 ValueSet via Parameters or ValueSet POST body",
				"operationId": "expandValueSetPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Expanded FHIR R4 ValueSet resource"},
				},
			},
		},
		"/ValueSet/{id}/$expand": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Terminology Services"},
				"summary":     "Expand a FHIR R4 ValueSet by logical ID",
				"operationId": "expandValueSetByIdGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "filter", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "count", "in": "query", "required": false, "schema": map[string]any{"type": "integer"}},
					{"name": "offset", "in": "query", "required": false, "schema": map[string]any{"type": "integer"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Expanded FHIR R4 ValueSet resource"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Terminology Services"},
				"summary":     "Expand a FHIR R4 ValueSet by logical ID (POST)",
				"operationId": "expandValueSetByIdPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Expanded FHIR R4 ValueSet resource"},
				},
			},
		},
		"/ValueSet/$validate-code": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Terminology Services"},
				"summary":     "Validate that a coded concept is a member of a FHIR R4 ValueSet",
				"operationId": "validateCodeValueSetGet",
				"parameters": []map[string]any{
					{"name": "url", "in": "query", "required": false, "description": "Canonical ValueSet URL", "schema": map[string]any{"type": "string"}},
					{"name": "code", "in": "query", "required": true, "description": "Code to validate", "schema": map[string]any{"type": "string"}},
					{"name": "system", "in": "query", "required": false, "description": "CodeSystem URI", "schema": map[string]any{"type": "string"}},
					{"name": "display", "in": "query", "required": false, "description": "Expected display string", "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters (result, message, display)"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Terminology Services"},
				"summary":     "Validate that a coded concept is a member of a FHIR R4 ValueSet (POST Parameters)",
				"operationId": "validateCodeValueSetPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters (result, message, display)"},
				},
			},
		},
		"/ValueSet/{id}/$validate-code": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Terminology Services"},
				"summary":     "Validate that a coded concept belongs to ValueSet/{id}",
				"operationId": "validateCodeValueSetByIdGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "code", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "system", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "display", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters (result, message, display)"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Terminology Services"},
				"summary":     "Validate that a coded concept belongs to ValueSet/{id} (POST Parameters)",
				"operationId": "validateCodeValueSetByIdPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters (result, message, display)"},
				},
			},
		},
		"/CodeSystem/$lookup": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Terminology Services"},
				"summary":     "Look up concept display, definition, and designations in a FHIR R4 CodeSystem",
				"operationId": "lookupCodeSystemGet",
				"parameters": []map[string]any{
					{"name": "system", "in": "query", "required": false, "description": "Canonical CodeSystem URL (e.g. http://loinc.org)", "schema": map[string]any{"type": "string"}},
					{"name": "code", "in": "query", "required": true, "description": "Concept code to look up", "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters (name, display, designation)"},
					"404": map[string]any{"description": "Concept or CodeSystem not found"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Terminology Services"},
				"summary":     "Look up concept in a FHIR R4 CodeSystem (POST Parameters)",
				"operationId": "lookupCodeSystemPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters (name, display, designation)"},
				},
			},
		},
		"/CodeSystem/{id}/$lookup": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Terminology Services"},
				"summary":     "Look up concept in CodeSystem/{id}",
				"operationId": "lookupCodeSystemByIdGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "code", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters (name, display, designation)"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Terminology Services"},
				"summary":     "Look up concept in CodeSystem/{id} (POST Parameters)",
				"operationId": "lookupCodeSystemByIdPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters (name, display, designation)"},
				},
			},
		},
		"/CodeSystem/$validate-code": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Terminology Services"},
				"summary":     "Validate that a coded concept belongs to a FHIR R4 CodeSystem ($validate-code)",
				"operationId": "validateCodeCodeSystemGet",
				"parameters": []map[string]any{
					{"name": "url", "in": "query", "required": false, "description": "Canonical CodeSystem URL (e.g. http://snomed.info/sct)", "schema": map[string]any{"type": "string"}},
					{"name": "code", "in": "query", "required": true, "description": "Concept code to validate", "schema": map[string]any{"type": "string"}},
					{"name": "display", "in": "query", "required": false, "description": "Expected display text", "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters (result, message, display)"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Terminology Services"},
				"summary":     "Validate that a coded concept belongs to a FHIR R4 CodeSystem (POST Parameters)",
				"operationId": "validateCodeCodeSystemPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters (result, message, display)"},
				},
			},
		},
		"/CodeSystem/{id}/$validate-code": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Terminology Services"},
				"summary":     "Validate that a coded concept belongs to CodeSystem/{id} ($validate-code)",
				"operationId": "validateCodeCodeSystemByIdGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "code", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "display", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters (result, message, display)"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Terminology Services"},
				"summary":     "Validate that a coded concept belongs to CodeSystem/{id} (POST Parameters)",
				"operationId": "validateCodeCodeSystemByIdPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters (result, message, display)"},
				},
			},
		},
		"/CodeSystem/$subsumes": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Terminology Services"},
				"summary":     "Test the subsumption relationship between codeA and codeB in a CodeSystem ($subsumes)",
				"operationId": "subsumesCodeSystemGet",
				"parameters": []map[string]any{
					{"name": "system", "in": "query", "required": true, "description": "Canonical CodeSystem URL (e.g. http://snomed.info/sct)", "schema": map[string]any{"type": "string"}},
					{"name": "codeA", "in": "query", "required": true, "description": "First concept code", "schema": map[string]any{"type": "string"}},
					{"name": "codeB", "in": "query", "required": true, "description": "Second concept code", "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters (outcome = equivalent | subsumes | subsumed-by | not-subsumed)"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Terminology Services"},
				"summary":     "Test the subsumption relationship between codeA/codingA and codeB/codingB (POST Parameters)",
				"operationId": "subsumesCodeSystemPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters (outcome = equivalent | subsumes | subsumed-by | not-subsumed)"},
				},
			},
		},
		"/CodeSystem/{id}/$subsumes": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Terminology Services"},
				"summary":     "Test the subsumption relationship between codeA and codeB in CodeSystem/{id} ($subsumes)",
				"operationId": "subsumesCodeSystemByIdGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "codeA", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "codeB", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters (outcome = equivalent | subsumes | subsumed-by | not-subsumed)"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Terminology Services"},
				"summary":     "Test the subsumption relationship between codeA/codingA and codeB/codingB in CodeSystem/{id} (POST Parameters)",
				"operationId": "subsumesCodeSystemByIdPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters (outcome = equivalent | subsumes | subsumed-by | not-subsumed)"},
				},
			},
		},
		"/ConceptMap/$translate": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Terminology Services"},
				"summary":     "Translate a coded concept across terminology systems (e.g. SNOMED CT <-> ICD-10-CM, LOINC <-> SNOMED CT)",
				"operationId": "translateConceptMapGet",
				"parameters": []map[string]any{
					{"name": "url", "in": "query", "required": false, "description": "Canonical ConceptMap URL", "schema": map[string]any{"type": "string"}},
					{"name": "system", "in": "query", "required": false, "description": "Source CodeSystem URI", "schema": map[string]any{"type": "string"}},
					{"name": "code", "in": "query", "required": true, "description": "Source concept code", "schema": map[string]any{"type": "string"}},
					{"name": "targetsystem", "in": "query", "required": false, "description": "Target CodeSystem URI", "schema": map[string]any{"type": "string"}},
					{"name": "reverse", "in": "query", "required": false, "schema": map[string]any{"type": "boolean"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters (result, message, match)"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Terminology Services"},
				"summary":     "Translate a coded concept across terminology systems (POST Parameters)",
				"operationId": "translateConceptMapPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters (result, message, match)"},
				},
			},
		},
		"/ConceptMap/{id}/$translate": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Terminology Services"},
				"summary":     "Translate a coded concept using ConceptMap/{id}",
				"operationId": "translateConceptMapByIdGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "code", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "system", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "reverse", "in": "query", "required": false, "schema": map[string]any{"type": "boolean"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters (result, message, match)"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Terminology Services"},
				"summary":     "Translate a coded concept using ConceptMap/{id} (POST Parameters)",
				"operationId": "translateConceptMapByIdPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters (result, message, match)"},
				},
			},
		},
		"/$export": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Bulk Data ($export)"},
				"summary":     "System-level FHIR Bulk Data Export (ONC (g)(10) NDJSON)",
				"operationId": "systemBulkExport",
				"parameters": []map[string]any{
					{"name": "_type", "in": "query", "description": "Comma-separated FHIR resource types (e.g. Patient,Observation)", "schema": map[string]any{"type": "string"}},
					{"name": "_since", "in": "query", "description": "Only include resources updated at or after this ISO-8601 timestamp", "schema": map[string]any{"type": "string"}},
					{"name": "_typeFilter", "in": "query", "description": "Comma-separated FHIR search criteria filters (e.g. Observation?category=vital-signs)", "schema": map[string]any{"type": "string"}},
					{"name": "_outputFormat", "in": "query", "description": "NDJSON output format (default application/fhir+ndjson)", "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Newline-delimited FHIR JSON (application/fhir+ndjson) or BulkExportManifest"},
					"202": map[string]any{"description": "Async job accepted (Content-Location polling URL)"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Bulk Data ($export)"},
				"summary":     "System-level FHIR Bulk Data Export via POST Parameters kick-off",
				"operationId": "systemBulkExportPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Newline-delimited FHIR JSON or BulkExportManifest"},
					"202": map[string]any{"description": "Async job accepted (Content-Location polling URL)"},
				},
			},
		},
		"/Patient/$export": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Bulk Data ($export)"},
				"summary":     "Patient-level Cohort Bulk Data Export (ONC (g)(10) NDJSON)",
				"operationId": "patientGroupBulkExport",
				"parameters": []map[string]any{
					{"name": "_type", "in": "query", "description": "Comma-separated FHIR resource types", "schema": map[string]any{"type": "string"}},
					{"name": "_since", "in": "query", "description": "ISO-8601 timestamp lower bound", "schema": map[string]any{"type": "string"}},
					{"name": "_typeFilter", "in": "query", "description": "Comma-separated FHIR search criteria filters", "schema": map[string]any{"type": "string"}},
					{"name": "patient", "in": "query", "description": "Optional patient cohort filter (e.g. Patient/pat-1,Patient/pat-2)", "schema": map[string]any{"type": "string"}},
					{"name": "_outputFormat", "in": "query", "description": "NDJSON output format (application/fhir+ndjson)", "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Newline-delimited FHIR JSON (application/fhir+ndjson)"},
					"202": map[string]any{"description": "Async job accepted (Content-Location polling URL)"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Bulk Data ($export)"},
				"summary":     "Patient-level Cohort Bulk Data Export via POST Parameters kick-off",
				"operationId": "patientGroupBulkExportPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Newline-delimited FHIR JSON or BulkExportManifest"},
					"202": map[string]any{"description": "Async job accepted (Content-Location polling URL)"},
				},
			},
		},
		"/Group/{id}/$export": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Bulk Data ($export)", "Group"},
				"summary":     "ONC (g)(10) Group Cohort Bulk Data Export resolving active Group.member Patients",
				"operationId": "groupBulkExport",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "_type", "in": "query", "description": "Comma-separated FHIR resource types", "schema": map[string]any{"type": "string"}},
					{"name": "_since", "in": "query", "description": "ISO-8601 timestamp lower bound", "schema": map[string]any{"type": "string"}},
					{"name": "_typeFilter", "in": "query", "description": "Comma-separated FHIR search criteria filters", "schema": map[string]any{"type": "string"}},
					{"name": "patient", "in": "query", "description": "Optional patient subset filter within the Group cohort", "schema": map[string]any{"type": "string"}},
					{"name": "_outputFormat", "in": "query", "description": "NDJSON output format (application/fhir+ndjson)", "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Newline-delimited FHIR JSON (application/fhir+ndjson) or BulkExportManifest"},
					"202": map[string]any{"description": "Async job accepted (Content-Location polling URL)"},
					"404": map[string]any{"description": "Group not found"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Bulk Data ($export)", "Group"},
				"summary":     "ONC (g)(10) Group Cohort Bulk Data Export via POST Parameters kick-off",
				"operationId": "groupBulkExportPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Newline-delimited FHIR JSON or BulkExportManifest"},
					"202": map[string]any{"description": "Async job accepted (Content-Location polling URL)"},
				},
			},
		},
		"/Patient/{id}/$export": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Bulk Data ($export)"},
				"summary":     "Single-Patient Compartment Bulk Data Export (NDJSON)",
				"operationId": "singlePatientBulkExport",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "_type", "in": "query", "description": "Comma-separated FHIR resource types", "schema": map[string]any{"type": "string"}},
					{"name": "_since", "in": "query", "description": "ISO-8601 timestamp lower bound", "schema": map[string]any{"type": "string"}},
					{"name": "_typeFilter", "in": "query", "description": "Comma-separated FHIR search criteria filters", "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Newline-delimited FHIR JSON (application/fhir+ndjson)"},
					"202": map[string]any{"description": "Async job accepted"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Bulk Data ($export)"},
				"summary":     "Single-Patient Compartment Bulk Data Export via POST Parameters kick-off",
				"operationId": "singlePatientBulkExportPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Newline-delimited FHIR JSON or BulkExportManifest"},
					"202": map[string]any{"description": "Async job accepted"},
				},
			},
		},
		"/Subscription/$status": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Subscription"},
				"summary":     "Query FHIR R4 Subscription status and delivery telemetry across active subscriptions ($status)",
				"operationId": "subscriptionStatusAllGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "query", "required": false, "description": "Optional Subscription ID", "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle of FHIR Parameters subscription status reports (or single Parameters if id specified)"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Subscription"},
				"summary":     "Query FHIR R4 Subscription status via POST Parameters ($status)",
				"operationId": "subscriptionStatusAllPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle of FHIR Parameters subscription status reports"},
				},
			},
		},
		"/Subscription/{id}/$status": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Subscription"},
				"summary":     "Query FHIR R4 Subscription/{id} status, events-since-subscription-start, and error diagnostics ($status)",
				"operationId": "subscriptionStatusGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters subscription status report"},
					"404": map[string]any{"description": "Subscription not found"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Subscription"},
				"summary":     "Query FHIR R4 Subscription/{id} status ($status, POST)",
				"operationId": "subscriptionStatusPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters subscription status report"},
				},
			},
		},
		"/$export-poll/{jobId}": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Bulk Data ($export)"},
				"summary":     "Poll Bulk Data Export Job Manifest",
				"operationId": "pollBulkExportJob",
				"parameters": []map[string]any{
					{"name": "jobId", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "BulkExportManifest JSON"},
				},
			},
			"delete": map[string]any{
				"tags":        []string{"Bulk Data ($export)"},
				"summary":     "Cancel / Delete Bulk Data Export Job",
				"operationId": "cancelBulkExportJob",
				"parameters": []map[string]any{
					{"name": "jobId", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"202": map[string]any{"description": "Export job deleted"},
				},
			},
		},
		"/$export-file/{jobId}/{fileName}": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Bulk Data ($export)"},
				"summary":     "Download Bulk Data Export NDJSON File",
				"operationId": "downloadBulkExportFile",
				"parameters": []map[string]any{
					{"name": "jobId", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "fileName", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "application/fhir+ndjson stream"},
				},
			},
		},
		"/$import": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Bulk Data ($export)"},
				"summary":     "HL7 Bulk Data Access NDJSON Import ($import — synchronous OperationOutcome or Prefer: respond-async 202 job)",
				"operationId": "systemBulkImport",
				"responses": map[string]any{
					"200": map[string]any{"description": "OperationOutcome summarizing imported FHIR resources"},
					"202": map[string]any{"description": "Async import job accepted (Content-Location polling URL)"},
				},
			},
		},
		"/$import-poll/{jobId}": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Bulk Data ($export)"},
				"summary":     "Poll Bulk Data Import Job Status & Manifest",
				"operationId": "pollBulkImportJob",
				"parameters": []map[string]any{
					{"name": "jobId", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "BulkImportManifest JSON"},
				},
			},
			"delete": map[string]any{
				"tags":        []string{"Bulk Data ($export)"},
				"summary":     "Cancel / Delete Bulk Data Import Job",
				"operationId": "cancelBulkImportJob",
				"parameters": []map[string]any{
					{"name": "jobId", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"202": map[string]any{"description": "Import job deleted"},
				},
			},
		},
		"/Patient/{id}/$purge": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Patient"},
				"summary":     "Purge a Patient and all clinical resources in their Patient compartment ($purge)",
				"operationId": "patientPurge",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "expunge", "in": "query", "required": false, "description": "Physically remove resources, history, and indexes when true", "schema": map[string]any{"type": "boolean"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "OperationOutcome summarizing purged compartment resources"},
					"404": map[string]any{"description": "Patient not found"},
				},
			},
		},
		"/$expunge": map[string]any{
			"post": map[string]any{
				"tags":        []string{"System Operations"},
				"summary":     "Physically expunge soft-deleted resources and/or historical versions across the tenant ($expunge)",
				"operationId": "systemExpunge",
				"parameters": []map[string]any{
					{"name": "expungeDeletedResources", "in": "query", "required": false, "schema": map[string]any{"type": "boolean"}},
					{"name": "expungePreviousVersions", "in": "query", "required": false, "schema": map[string]any{"type": "boolean"}},
					{"name": "expungeEverything", "in": "query", "required": false, "schema": map[string]any{"type": "boolean"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "OperationOutcome summarizing expunged records"},
				},
			},
		},
		"/$diff": map[string]any{
			"post": map[string]any{
				"tags":        []string{"System Operations"},
				"summary":     "Compute structural FHIR Parameters diff between two inline FHIR resources ($diff)",
				"operationId": "systemDiff",
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters patch document"},
				},
			},
		},
		"/Patient/$match": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Patient"},
				"summary":     "Master Patient Index (MPI) probabilistic & deterministic patient matching ($match)",
				"operationId": "patientMatch",
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: searchset) with match-grade and score"},
					"400": map[string]any{"description": "Invalid Parameters input"},
				},
			},
		},
		"/Patient/$member-match": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Patient"},
				"summary":     "HL7 Da Vinci PDex/HRex $member-match operation matching MemberPatient and CoverageToMatch",
				"operationId": "patientMemberMatch",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing matched MemberIdentifier and Patient reference"},
					"422": map[string]any{"description": "Zero or multiple ambiguous member matches"},
				},
			},
		},
		"/Patient/{id}/$summary": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient"},
				"summary":     "Generate HL7 International Patient Summary (IPS) document Bundle ($summary, LOINC 60591-5)",
				"operationId": "patientSummaryGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: document) starting with IPS Composition"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Patient"},
				"summary":     "Generate HL7 International Patient Summary (IPS) document Bundle ($summary, POST)",
				"operationId": "patientSummaryPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: document) starting with IPS Composition"},
				},
			},
		},
		"/DocumentReference/$docref": map[string]any{
			"get": map[string]any{
				"tags":        []string{"DocumentReference"},
				"summary":     "US Core / Argonaut Clinical Notes $docref operation returning matching DocumentReferences and optional on-demand IPS summary",
				"operationId": "documentReferenceDocrefGet",
				"parameters": []map[string]any{
					{"name": "patient", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "start", "in": "query", "required": false, "schema": map[string]any{"type": "string", "format": "date"}},
					{"name": "end", "in": "query", "required": false, "schema": map[string]any{"type": "string", "format": "date"}},
					{"name": "type", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "on-demand", "in": "query", "required": false, "schema": map[string]any{"type": "boolean"}},
					{"name": "_count", "in": "query", "required": false, "schema": map[string]any{"type": "integer"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of DocumentReference resources"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"DocumentReference"},
				"summary":     "US Core / Argonaut Clinical Notes $docref operation (POST Parameters)",
				"operationId": "documentReferenceDocrefPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of DocumentReference resources"},
				},
			},
		},
		"/Composition/{id}/$document": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Composition"},
				"summary":     "Generate FHIR R4 clinical document Bundle ($document) from a Composition and its referenced resources",
				"operationId": "compositionDocumentGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "persist", "in": "query", "required": false, "schema": map[string]any{"type": "boolean"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: document) starting with Composition"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Composition"},
				"summary":     "Generate FHIR R4 clinical document Bundle ($document, POST)",
				"operationId": "compositionDocumentPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: document) starting with Composition"},
				},
			},
		},
		"/Questionnaire/$populate": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Questionnaire"},
				"summary":     "HL7 Da Vinci SDC / DTR Questionnaire/$populate pre-populating a QuestionnaireResponse from patient context",
				"operationId": "questionnairePopulateGet",
				"parameters": []map[string]any{
					{"name": "identifier", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "subject", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Pre-populated FHIR R4 QuestionnaireResponse"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Questionnaire"},
				"summary":     "HL7 Da Vinci SDC / DTR Questionnaire/$populate (POST Parameters)",
				"operationId": "questionnairePopulatePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Pre-populated FHIR R4 QuestionnaireResponse"},
				},
			},
		},
		"/Questionnaire/{id}/$populate": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Questionnaire"},
				"summary":     "HL7 Da Vinci SDC / DTR Questionnaire/{id}/$populate",
				"operationId": "questionnairePopulateByIdGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "subject", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Pre-populated FHIR R4 QuestionnaireResponse"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Questionnaire"},
				"summary":     "HL7 Da Vinci SDC / DTR Questionnaire/{id}/$populate (POST Parameters)",
				"operationId": "questionnairePopulateByIdPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Pre-populated FHIR R4 QuestionnaireResponse"},
				},
			},
		},
		"/QuestionnaireResponse/$extract": map[string]any{
			"post": map[string]any{
				"tags":        []string{"QuestionnaireResponse"},
				"summary":     "HL7 SDC QuestionnaireResponse/$extract converting coded answers into FHIR Observation / Condition / Provenance transaction Bundle",
				"operationId": "questionnaireResponseExtractPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Extracted FHIR R4 Bundle (type: transaction)"},
				},
			},
		},
		"/QuestionnaireResponse/{id}/$extract": map[string]any{
			"post": map[string]any{
				"tags":        []string{"QuestionnaireResponse"},
				"summary":     "HL7 SDC QuestionnaireResponse/{id}/$extract",
				"operationId": "questionnaireResponseExtractByIdPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Extracted FHIR R4 Bundle (type: transaction)"},
				},
			},
		},
		"/Claim/$submit": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Claim"},
				"summary":     "HL7 Da Vinci PAS / FHIR R4 Claim/$submit real-time claim & prior authorization adjudication",
				"operationId": "claimSubmit",
				"responses": map[string]any{
					"200": map[string]any{"description": "Adjudicated FHIR R4 ClaimResponse resource"},
					"400": map[string]any{"description": "Validation error OperationOutcome"},
				},
			},
		},
		"/Patient/{id}/$everything": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient"},
				"summary":     "Assemble full Patient compartment longitudinal Bundle ($everything)",
				"operationId": "patientEverything",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "_type", "in": "query", "description": "Comma-separated resource types", "schema": map[string]any{"type": "string"}},
					{"name": "_since", "in": "query", "description": "ISO-8601 timestamp lower bound", "schema": map[string]any{"type": "string"}},
					{"name": "_typeFilter", "in": "query", "description": "Comma-separated FHIR search criteria filters (e.g. Observation?category=vital-signs)", "schema": map[string]any{"type": "string"}},
					{"name": "_count", "in": "query", "schema": map[string]any{"type": "integer"}},
					{"name": "_offset", "in": "query", "schema": map[string]any{"type": "integer"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: searchset) containing Patient and compartment resources"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Patient"},
				"summary":     "Assemble full Patient compartment longitudinal Bundle ($everything, POST Parameters)",
				"operationId": "patientEverythingPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: searchset) containing Patient and compartment resources"},
				},
			},
		},
		"/Encounter/{id}/$everything": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Encounter"},
				"summary":     "Assemble full Encounter compartment Bundle ($everything)",
				"operationId": "encounterEverything",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "_type", "in": "query", "description": "Comma-separated resource types", "schema": map[string]any{"type": "string"}},
					{"name": "_since", "in": "query", "description": "ISO-8601 timestamp lower bound", "schema": map[string]any{"type": "string"}},
					{"name": "_typeFilter", "in": "query", "description": "Comma-separated FHIR search criteria filters (e.g. Observation?category=vital-signs)", "schema": map[string]any{"type": "string"}},
					{"name": "_count", "in": "query", "schema": map[string]any{"type": "integer"}},
					{"name": "_offset", "in": "query", "schema": map[string]any{"type": "integer"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: searchset) containing Encounter, clinical resources, and referenced participants/locations"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Encounter"},
				"summary":     "Assemble full Encounter compartment Bundle ($everything, POST Parameters)",
				"operationId": "encounterEverythingPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: searchset) containing Encounter, clinical resources, and referenced participants/locations"},
				},
			},
		},
		"/AuditEvent": map[string]any{
			"get": map[string]any{
				"tags":        []string{"AuditEvent"},
				"summary":     "Search HIPAA / ONC Security AuditEvent Records",
				"operationId": "searchAuditEvents",
				"parameters": []map[string]any{
					{"name": "action", "in": "query", "schema": map[string]any{"type": "string"}},
					{"name": "subtype", "in": "query", "schema": map[string]any{"type": "string"}},
					{"name": "outcome", "in": "query", "schema": map[string]any{"type": "string"}},
					{"name": "agent", "in": "query", "schema": map[string]any{"type": "string"}},
					{"name": "patient", "in": "query", "schema": map[string]any{"type": "string"}},
					{"name": "entity-type", "in": "query", "schema": map[string]any{"type": "string"}},
					{"name": "entity-id", "in": "query", "schema": map[string]any{"type": "string"}},
					{"name": "_count", "in": "query", "schema": map[string]any{"type": "integer"}},
					{"name": "_offset", "in": "query", "schema": map[string]any{"type": "integer"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle of FHIR AuditEvent resources"},
				},
			},
		},
		"/AuditEvent/_search": map[string]any{
			"post": map[string]any{
				"tags":        []string{"AuditEvent"},
				"summary":     "Search HIPAA / ONC Security AuditEvent Records (POST _search)",
				"operationId": "postSearchAuditEvents",
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle of FHIR AuditEvent resources"},
				},
			},
		},
		"/AuditEvent/{id}": map[string]any{
			"get": map[string]any{
				"tags":        []string{"AuditEvent"},
				"summary":     "Read a single HIPAA / ONC AuditEvent by ID",
				"operationId": "readAuditEvent",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR AuditEvent resource"},
				},
			},
		},
	}

	spDefs := resourceSearchParamDefs()
	for _, rt := range resOrder {
		queryParams := []map[string]any{
			{"name": "_id", "in": "query", "description": "Logical resource ID", "schema": map[string]any{"type": "string"}},
			{"name": "_lastUpdated", "in": "query", "description": "Last updated timestamp with optional FHIR comparator (ge, le, gt, lt)", "schema": map[string]any{"type": "string"}},
			{"name": "_profile", "in": "query", "description": "Resource profile canonical URL (supports :below, :above, :missing)", "schema": map[string]any{"type": "string"}},
			{"name": "_tag", "in": "query", "description": "Resource meta.tag token (system|code, supports :not, :missing)", "schema": map[string]any{"type": "string"}},
			{"name": "_security", "in": "query", "description": "Resource meta.security label token (system|code, supports :not, :missing)", "schema": map[string]any{"type": "string"}},
			{"name": "_source", "in": "query", "description": "Resource meta.source URI (supports :below, :above, :missing)", "schema": map[string]any{"type": "string"}},
			{"name": "_count", "in": "query", "description": "Page size (default 100)", "schema": map[string]any{"type": "integer"}},
			{"name": "_offset", "in": "query", "description": "Pagination offset", "schema": map[string]any{"type": "integer"}},
			{"name": "_sort", "in": "query", "description": "Comma-separated sort fields with optional '-' descending prefix (e.g. -date,_id)", "schema": map[string]any{"type": "string"}},
			{"name": "_total", "in": "query", "description": "Total count mode (accurate or none)", "schema": map[string]any{"type": "string", "enum": []string{"accurate", "none"}}},
			{"name": "_summary", "in": "query", "description": "Summary projection mode (true, text, data, count, false)", "schema": map[string]any{"type": "string", "enum": []string{"true", "text", "data", "count", "false"}}},
			{"name": "_elements", "in": "query", "description": "Comma-separated top-level FHIR elements to project (adds SUBSETTED tag)", "schema": map[string]any{"type": "string"}},
			{"name": "_include", "in": "query", "description": "Forward reference include (e.g. Observation:patient)", "schema": map[string]any{"type": "string"}},
			{"name": "_revinclude", "in": "query", "description": "Reverse reference include (e.g. Observation:patient)", "schema": map[string]any{"type": "string"}},
			{"name": "_include:iterate", "in": "query", "description": "Iterative multi-hop forward reference include", "schema": map[string]any{"type": "string"}},
			{"name": "_revinclude:iterate", "in": "query", "description": "Iterative multi-hop reverse reference include", "schema": map[string]any{"type": "string"}},
			{"name": "_has", "in": "query", "description": "Reverse chained search filter (e.g. _has:Observation:patient:code=8867-4)", "schema": map[string]any{"type": "string"}},
			{"name": "_list", "in": "query", "description": "Cohort membership search against a FHIR List or Group ID", "schema": map[string]any{"type": "string"}},
		}
		seenParam := map[string]bool{"_id": true, "_lastUpdated": true, "_profile": true, "_tag": true, "_security": true, "_source": true}
		for _, sp := range spDefs[rt] {
			if seenParam[sp["name"]] {
				continue
			}
			seenParam[sp["name"]] = true
			queryParams = append(queryParams, map[string]any{
				"name":        sp["name"],
				"in":          "query",
				"description": fmt.Sprintf("FHIR %s search parameter (%s)", sp["type"], sp["name"]),
				"schema":      map[string]any{"type": "string"},
			})
		}

		paths["/"+rt] = map[string]any{
			"get": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Search %s resources via covering spidx_* indexes", rt),
				"operationId": "search" + rt,
				"parameters":  queryParams,
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of " + rt + " resources"},
				},
			},
			"post": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Create %s resource (supports If-None-Exist conditional create and X-Provenance header)", rt),
				"operationId": "create" + rt,
				"parameters": []map[string]any{
					{"name": "If-None-Exist", "in": "header", "required": false, "description": "FHIR conditional create search query (e.g. identifier=http://hospital.example.org/mrn|MRN-001)", "schema": map[string]any{"type": "string"}},
					{"name": "X-Provenance", "in": "header", "required": false, "description": "Automatically record US Core Provenance (true or JSON/base64 Provenance template)", "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"201": map[string]any{"description": "Created " + rt + " (with ETag, Location, and optional X-Provenance-Location headers)"},
					"200": map[string]any{"description": "Idempotent match via If-None-Exist"},
					"400": map[string]any{"description": "Validation failure OperationOutcome"},
					"412": map[string]any{"description": "Multiple matches for If-None-Exist"},
				},
			},
			"put": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Conditional Update %s via search query parameters", rt),
				"operationId": "conditionalUpdate" + rt,
				"parameters":  queryParams,
				"responses": map[string]any{
					"200": map[string]any{"description": "Updated single matching " + rt},
					"201": map[string]any{"description": "Created new " + rt + " (zero matches)"},
					"412": map[string]any{"description": "Precondition failed (multiple matches or If-Match conflict)"},
				},
			},
			"patch": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Conditional Patch %s via search query parameters (RFC 6902, RFC 7396, or FHIRPath Patch Parameters)", rt),
				"operationId": "conditionalPatch" + rt,
				"parameters":  queryParams,
				"responses": map[string]any{
					"200": map[string]any{"description": "Patched single matching " + rt},
					"404": map[string]any{"description": "Zero matching " + rt + " resources"},
					"412": map[string]any{"description": "Precondition failed (multiple matches or If-Match conflict)"},
				},
			},
			"delete": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Conditional Delete %s via search query parameters", rt),
				"operationId": "conditionalDelete" + rt,
				"parameters":  queryParams,
				"responses": map[string]any{
					"204": map[string]any{"description": "Deleted matching " + rt + " (or no-op if zero matches)"},
					"412": map[string]any{"description": "Precondition failed (multiple matches)"},
				},
			},
		}

		paths["/"+rt+"/_search"] = map[string]any{
			"post": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Search %s resources via POST form body or query parameters", rt),
				"operationId": "postSearch" + rt,
				"parameters":  queryParams,
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of " + rt + " resources"},
				},
			},
		}

		paths["/"+rt+"/_history"] = map[string]any{
			"get": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("List type-level version history for all %s resources (supports _since, _at, _list, _summary, _elements)", rt),
				"operationId": "typeHistory" + rt,
				"parameters": []map[string]any{
					{"name": "_count", "in": "query", "schema": map[string]any{"type": "integer"}},
					{"name": "_offset", "in": "query", "schema": map[string]any{"type": "integer"}},
					{"name": "_since", "in": "query", "description": "ISO-8601 lower bound timestamp", "schema": map[string]any{"type": "string"}},
					{"name": "_at", "in": "query", "description": "Point-in-time or period currency filter (with optional comparator prefix eq, lt, le, gt, ge)", "schema": map[string]any{"type": "string"}},
					{"name": "_list", "in": "query", "description": "Cohort List or Group ID filter", "schema": map[string]any{"type": "string"}},
					{"name": "_summary", "in": "query", "description": "Summary projection mode (true, text, data, count, false)", "schema": map[string]any{"type": "string"}},
					{"name": "_elements", "in": "query", "description": "Comma-separated top-level elements to project", "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "History Bundle (type: history)"},
				},
			},
		}

		if postgres.IsCompartmentType(rt) {
			paths["/"+rt+"/{id}/*"] = map[string]any{
				"get": map[string]any{
					"tags":        []string{rt},
					"summary":     fmt.Sprintf("Wildcard multi-type search across all resources in %s/{id} compartment", rt),
					"operationId": "searchWildcard" + rt + "Compartment",
					"parameters": []map[string]any{
						{"name": "id", "in": "path", "required": true, "description": rt + " logical ID", "schema": map[string]any{"type": "string"}},
						{"name": "_type", "in": "query", "required": false, "description": "Optional comma-separated FHIR resource types", "schema": map[string]any{"type": "string"}},
						{"name": "_count", "in": "query", "required": false, "schema": map[string]any{"type": "integer"}},
						{"name": "_offset", "in": "query", "required": false, "schema": map[string]any{"type": "integer"}},
					},
					"responses": map[string]any{
						"200": map[string]any{"description": "Searchset Bundle of resources in " + rt + "/{id} compartment"},
					},
				},
			}
			paths["/"+rt+"/{id}/*/_search"] = map[string]any{
				"post": map[string]any{
					"tags":        []string{rt},
					"summary":     fmt.Sprintf("Wildcard multi-type search across all resources in %s/{id} compartment (POST _search)", rt),
					"operationId": "postSearchWildcard" + rt + "Compartment",
					"parameters": []map[string]any{
						{"name": "id", "in": "path", "required": true, "description": rt + " logical ID", "schema": map[string]any{"type": "string"}},
					},
					"responses": map[string]any{
						"200": map[string]any{"description": "Searchset Bundle of resources in " + rt + "/{id} compartment"},
					},
				},
			}
		}

		if postgres.IsPatientCompartmentResourceType(rt) {
			compParams := append([]map[string]any{
				{"name": "id", "in": "path", "required": true, "description": "Patient logical ID", "schema": map[string]any{"type": "string"}},
			}, queryParams...)
			paths["/Patient/{id}/"+rt] = map[string]any{
				"get": map[string]any{
					"tags":        []string{"Patient", rt},
					"summary":     fmt.Sprintf("Search %s resources within Patient/{id} compartment", rt),
					"operationId": "searchPatientCompartment" + rt,
					"parameters":  compParams,
					"responses": map[string]any{
						"200": map[string]any{"description": "Searchset Bundle of " + rt + " resources for Patient/{id}"},
					},
				},
			}
		}

		paths["/"+rt+"/{id}"] = map[string]any{
			"get": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Read %s by ID (supports If-None-Match and If-Modified-Since conditional read)", rt),
				"operationId": "read" + rt,
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "If-None-Match", "in": "header", "required": false, "description": `Expected version ETag (e.g. W/"1" or *) for conditional read`, "schema": map[string]any{"type": "string"}},
					{"name": "If-Modified-Since", "in": "header", "required": false, "description": "HTTP-date or RFC3339 timestamp for conditional read", "schema": map[string]any{"type": "string"}},
					{"name": "_summary", "in": "query", "required": false, "description": "Summary projection mode (true, text, data, false)", "schema": map[string]any{"type": "string"}},
					{"name": "_elements", "in": "query", "required": false, "description": "Comma-separated top-level elements to project", "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": rt + " resource with weak ETag and Last-Modified headers"},
					"304": map[string]any{"description": "Not Modified (Conditional Read satisfied)"},
					"404": map[string]any{"description": "Not found"},
					"410": map[string]any{"description": "Soft-deleted tombstone"},
				},
			},
			"put": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Update or Create-on-Update %s (supports If-Match CAS and X-Provenance)", rt),
				"operationId": "update" + rt,
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "If-Match", "in": "header", "required": false, "description": `Expected version ETag (e.g. W/"1") for optimistic concurrency control`, "schema": map[string]any{"type": "string"}},
					{"name": "X-Provenance", "in": "header", "required": false, "description": "Automatically record US Core Provenance (true or JSON/base64 Provenance template)", "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Updated " + rt},
					"201": map[string]any{"description": "Created " + rt},
					"412": map[string]any{"description": "If-Match version conflict"},
				},
			},
			"patch": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Patch %s via JSON Patch (RFC 6902) or JSON Merge Patch (RFC 7396)", rt),
				"operationId": "patch" + rt,
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "If-Match", "in": "header", "required": false, "description": `Expected version ETag (e.g. W/"1") for optimistic concurrency control`, "schema": map[string]any{"type": "string"}},
					{"name": "X-Provenance", "in": "header", "required": false, "description": "Automatically record US Core Provenance (true or JSON/base64 Provenance template)", "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Patched " + rt},
					"400": map[string]any{"description": "Malformed patch or StructureDefinition validation error"},
					"412": map[string]any{"description": "If-Match version conflict"},
					"422": map[string]any{"description": "JSON Patch test or path precondition failed"},
				},
			},
			"delete": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Soft-delete %s and clear search indexes (supports If-Match CAS)", rt),
				"operationId": "delete" + rt,
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "If-Match", "in": "header", "required": false, "description": `Expected version ETag (e.g. W/"1") for optimistic concurrency control`, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"204": map[string]any{"description": "Deleted (tombstone version recorded in _history)"},
					"412": map[string]any{"description": "If-Match version conflict"},
				},
			},
		}

		paths["/"+rt+"/{id}/_history"] = map[string]any{
			"get": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("List version history for %s/{id} (supports _since, _at, _list, _summary, _elements)", rt),
				"operationId": "history" + rt,
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "_count", "in": "query", "schema": map[string]any{"type": "integer"}},
					{"name": "_offset", "in": "query", "schema": map[string]any{"type": "integer"}},
					{"name": "_since", "in": "query", "description": "ISO-8601 lower bound timestamp", "schema": map[string]any{"type": "string"}},
					{"name": "_at", "in": "query", "description": "Point-in-time or period currency filter (with optional comparator prefix eq, lt, le, gt, ge)", "schema": map[string]any{"type": "string"}},
					{"name": "_list", "in": "query", "description": "Cohort List or Group ID filter", "schema": map[string]any{"type": "string"}},
					{"name": "_summary", "in": "query", "description": "Summary projection mode (true, text, data, count, false)", "schema": map[string]any{"type": "string"}},
					{"name": "_elements", "in": "query", "description": "Comma-separated top-level elements to project", "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "History Bundle"},
				},
			},
		}

		paths["/"+rt+"/{id}/_history/{vid}"] = map[string]any{
			"get": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Read specific version (vread) of %s/{id} (supports _summary and _elements projection)", rt),
				"operationId": "vread" + rt,
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "vid", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "_summary", "in": "query", "required": false, "description": "Summary projection mode (true, text, data, false)", "schema": map[string]any{"type": "string"}},
					{"name": "_elements", "in": "query", "required": false, "description": "Comma-separated top-level elements to project", "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": rt + " historical version"},
					"410": map[string]any{"description": "Deleted tombstone version"},
				},
			},
		}

		paths["/"+rt+"/$validate"] = map[string]any{
			"post": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Validate %s resource against StructureDefinition, US Core profiles, & ValueSets", rt),
				"operationId": "validate" + rt,
				"parameters": []map[string]any{
					{"name": "profile", "in": "query", "required": false, "description": "StructureDefinition canonical URL (e.g. http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient)", "schema": map[string]any{"type": "string"}},
					{"name": "mode", "in": "query", "required": false, "description": "Validation mode (create, update, delete)", "schema": map[string]any{"type": "string", "enum": []string{"create", "update", "delete"}}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "OperationOutcome validation result"},
				},
			},
		}

		paths["/"+rt+"/{id}/$validate"] = map[string]any{
			"get": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Validate stored %s/{id} instance against StructureDefinition, US Core profiles, & ValueSets", rt),
				"operationId": "validateInstanceGet" + rt,
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "profile", "in": "query", "required": false, "description": "StructureDefinition canonical URL", "schema": map[string]any{"type": "string"}},
					{"name": "mode", "in": "query", "required": false, "description": "Validation mode (create, update, delete)", "schema": map[string]any{"type": "string", "enum": []string{"create", "update", "delete"}}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "OperationOutcome validation result"},
					"404": map[string]any{"description": rt + " not found"},
				},
			},
			"post": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Validate %s/{id} instance against StructureDefinition, US Core profiles, & ValueSets (POST)", rt),
				"operationId": "validateInstancePost" + rt,
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "profile", "in": "query", "required": false, "description": "StructureDefinition canonical URL", "schema": map[string]any{"type": "string"}},
					{"name": "mode", "in": "query", "required": false, "description": "Validation mode (create, update, delete)", "schema": map[string]any{"type": "string", "enum": []string{"create", "update", "delete"}}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "OperationOutcome validation result"},
				},
			},
		}

		paths["/"+rt+"/$diff"] = map[string]any{
			"post": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Compute structural FHIR Parameters diff between two %s resources ($diff)", rt),
				"operationId": "typeDiff" + rt,
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters patch document"},
				},
			},
		}

		paths["/"+rt+"/{id}/$diff"] = map[string]any{
			"get": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Compare historical versions of %s/{id} and return a FHIR Parameters patch ($diff)", rt),
				"operationId": "instanceDiffGet" + rt,
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "fromVersion", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "version", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters patch document"},
					"404": map[string]any{"description": rt + " or version not found"},
				},
			},
			"post": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Compare historical versions of %s/{id} ($diff, POST Parameters)", rt),
				"operationId": "instanceDiffPost" + rt,
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters patch document"},
				},
			},
		}

		paths["/"+rt+"/{id}/$graph"] = map[string]any{
			"get": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Assemble a clinical resource graph starting at %s/{id} via GraphDefinition ($graph)", rt),
				"operationId": "instanceGraphGet" + rt,
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "graph", "in": "query", "required": false, "description": "GraphDefinition ID, name, or canonical URL (e.g. us-core-patient-care-graph)", "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Collection Bundle containing " + rt + "/{id} and all GraphDefinition-linked resources"},
					"404": map[string]any{"description": rt + " or GraphDefinition not found"},
				},
			},
			"post": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Assemble a clinical resource graph starting at %s/{id} via inline GraphDefinition or Parameters ($graph)", rt),
				"operationId": "instanceGraphPost" + rt,
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Collection Bundle containing " + rt + "/{id} and all GraphDefinition-linked resources"},
					"422": map[string]any{"description": "GraphDefinition start type mismatch"},
				},
			},
		}

		paths["/"+rt+"/$expunge"] = map[string]any{
			"post": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Physically expunge soft-deleted %s resources and/or historical versions ($expunge)", rt),
				"operationId": "typeExpunge" + rt,
				"responses": map[string]any{
					"200": map[string]any{"description": "OperationOutcome summarizing expunged records"},
				},
			},
		}

		paths["/"+rt+"/{id}/$expunge"] = map[string]any{
			"post": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Physically expunge %s/{id} versions or deleted state ($expunge)", rt),
				"operationId": "instanceExpunge" + rt,
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "OperationOutcome summarizing expunged records"},
				},
			},
		}

		paths["/"+rt+"/$meta"] = map[string]any{
			"get": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Type-level summary of profiles, tags, and security labels for %s ($meta)", rt),
				"operationId": "typeMeta" + rt,
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters (return.valueMeta)"},
				},
			},
		}

		paths["/"+rt+"/{id}/$meta"] = map[string]any{
			"get": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Retrieve profile, tag, and security metadata for %s/{id} ($meta)", rt),
				"operationId": "instanceMeta" + rt,
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR Parameters (return.valueMeta)"},
				},
			},
		}

		paths["/"+rt+"/{id}/$meta-add"] = map[string]any{
			"post": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Add profile, tag, or security labels to %s/{id} ($meta-add)", rt),
				"operationId": "metaAdd" + rt,
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Updated FHIR Parameters (return.valueMeta)"},
				},
			},
		}

		paths["/"+rt+"/{id}/$meta-delete"] = map[string]any{
			"post": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Remove profile, tag, or security labels from %s/{id} ($meta-delete)", rt),
				"operationId": "metaDelete" + rt,
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Updated FHIR Parameters (return.valueMeta)"},
				},
			},
		}

		paths["/"+rt+"/{id}/$provenance"] = map[string]any{
			"get": map[string]any{
				"tags":        []string{rt, "Provenance"},
				"summary":     fmt.Sprintf("Retrieve US Core Provenance lineage Bundle targeting %s/{id} ($provenance)", rt),
				"operationId": "instanceProvenance" + rt,
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of Provenance resources targeting " + rt + "/{id}"},
				},
			},
		}
	}

	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "Flint FHIR R4 Server & Lakehouse API",
			"version":     "0.1.0",
			"description": "Proto-native HL7 FHIR R4 operational store (Zero JSONB in Postgres) with HAPI covering search indexes, Terminology Services ($expand, $validate-code, $lookup), FHIR Subscriptions (rest-hook), SMART on FHIR v2 authorization, HIPAA/ONC AuditEvent logging, Bulk Data ($export), and AutoMQ -> Iceberg -> SQLMesh lakehouse.",
			"contact": map[string]any{
				"name": "Flint FHIR Engineering",
				"url":  "https://github.com/flint-fhir/flint",
			},
		},
		"servers": []map[string]any{
			{
				"url":         baseURL,
				"description": fmt.Sprintf("Flint FHIR R4 Tenant Endpoint (%s)", tenant),
			},
		},
		"security": []map[string]any{
			{"SMARTBearerAuth": []string{}},
		},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"SMARTBearerAuth": map[string]any{
					"type":         "http",
					"scheme":       "bearer",
					"bearerFormat": "JWT",
					"description":  "SMART on FHIR v1/v2 OAuth2 JWT Bearer Token (e.g. system/*.rs, user/*.cruds, patient/Observation.rs?category=vital-signs)",
				},
			},
			"schemas": schemas,
		},
		"paths":               paths,
		"x-flint-conformance": ConformanceMatrix(),
	}
}

// handleOpenAPI serves GET /fhir/r4/openapi.json and GET /fhir/r4/{tenant}/openapi.json.
func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	tenant := strings.TrimSpace(r.PathValue("tenant"))
	if tenant == "" {
		tenant = strings.TrimSpace(r.URL.Query().Get("tenant"))
	}
	if tenant == "" {
		tenant = "demo"
	}

	spec := s.BuildOpenAPISpec(tenant)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(spec)
}

// handleDocsUI serves GET /fhir/r4/$docs and GET /fhir/r4/{tenant}/$docs — a self-contained
// interactive OpenAPI 3.1 & FHIR R4 Conformance Explorer with zero external runtime dependencies.
func (s *Server) handleDocsUI(w http.ResponseWriter, r *http.Request) {
	tenant := strings.TrimSpace(r.PathValue("tenant"))
	if tenant == "" {
		tenant = strings.TrimSpace(r.URL.Query().Get("tenant"))
	}
	if tenant == "" {
		tenant = "demo"
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	html := strings.ReplaceAll(docsHTMLTemplate, "{{TENANT}}", tenant)
	_, _ = w.Write([]byte(html))
}

const docsHTMLTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Flint FHIR R4 API, SMART v2 Sandbox &amp; Conformance Explorer ({{TENANT}})</title>
  <style>
    :root {
      --bg: #0f172a;
      --panel: #1e293b;
      --border: #334155;
      --text: #f8fafc;
      --muted: #94a3b8;
      --accent: #f97316;
      --green: #22c55e;
      --blue: #38bdf8;
      --purple: #a855f7;
      --red: #ef4444;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
      background: var(--bg);
      color: var(--text);
      line-height: 1.5;
    }
    header {
      background: #020617;
      border-bottom: 1px solid var(--border);
      padding: 1.25rem 2rem;
      display: flex;
      justify-content: space-between;
      align-items: center;
      flex-wrap: wrap;
      gap: 1rem;
    }
    .brand { display: flex; align-items: center; gap: 0.75rem; flex-wrap: wrap; }
    .brand h1 { margin: 0; font-size: 1.35rem; font-weight: 700; }
    .badge {
      background: rgba(249, 115, 22, 0.15);
      color: var(--accent);
      border: 1px solid rgba(249, 115, 22, 0.4);
      padding: 0.2rem 0.6rem;
      border-radius: 999px;
      font-size: 0.75rem;
      font-weight: 600;
    }
    .controls { display: flex; gap: 0.6rem; align-items: center; flex-wrap: wrap; }
    input[type="text"], select, textarea {
      background: var(--panel);
      border: 1px solid var(--border);
      color: var(--text);
      padding: 0.45rem 0.75rem;
      border-radius: 0.375rem;
      font-size: 0.85rem;
    }
    input[type="text"] { min-width: 240px; }
    textarea {
      width: 100%;
      min-height: 130px;
      font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
      font-size: 0.8rem;
      background: #020617;
    }
    .btn {
      background: var(--accent);
      color: #fff;
      border: none;
      padding: 0.45rem 0.85rem;
      border-radius: 0.375rem;
      font-weight: 600;
      font-size: 0.82rem;
      cursor: pointer;
      text-decoration: none;
    }
    .btn-secondary {
      background: #334155;
      color: var(--text);
    }
    .btn-green {
      background: #16a34a;
      color: #fff;
    }
    main { max-width: 1320px; margin: 0 auto; padding: 1.5rem 2rem; }
    h2 { font-size: 1.15rem; margin-top: 2rem; border-bottom: 1px solid var(--border); padding-bottom: 0.5rem; }
    table { width: 100%; border-collapse: collapse; background: var(--panel); border-radius: 0.5rem; overflow: hidden; font-size: 0.875rem; }
    th, td { text-align: left; padding: 0.65rem 0.9rem; border-bottom: 1px solid var(--border); }
    th { background: rgba(15, 23, 42, 0.6); color: var(--muted); font-weight: 600; }
    .status-ok { color: var(--green); font-weight: 600; }
    .workbench-panel {
      background: var(--panel);
      border: 1px solid var(--border);
      border-radius: 0.5rem;
      padding: 1rem;
      margin-bottom: 1.25rem;
    }
    .workbench-row {
      display: flex;
      gap: 0.6rem;
      align-items: center;
      flex-wrap: wrap;
      margin-bottom: 0.75rem;
    }
    .recipes-grid {
      display: grid;
      grid-template-columns: repeat(auto-fill, minmax(290px, 1fr));
      gap: 0.75rem;
      margin-bottom: 1.25rem;
    }
    .recipe-card {
      background: var(--panel);
      border: 1px solid var(--border);
      border-radius: 0.5rem;
      padding: 0.85rem 1rem;
      display: flex;
      flex-direction: column;
      justify-content: space-between;
      gap: 0.5rem;
    }
    .recipe-title { font-weight: 700; font-size: 0.9rem; color: var(--text); }
    .recipe-desc { font-size: 0.78rem; color: var(--muted); }
    .endpoint-card {
      background: var(--panel);
      border: 1px solid var(--border);
      border-radius: 0.5rem;
      margin-bottom: 0.75rem;
      padding: 0.85rem 1rem;
    }
    .endpoint-header { display: flex; align-items: center; gap: 0.75rem; justify-content: space-between; flex-wrap: wrap; }
    .method {
      font-weight: 700;
      font-size: 0.75rem;
      padding: 0.2rem 0.55rem;
      border-radius: 0.25rem;
      text-transform: uppercase;
      min-width: 64px;
      text-align: center;
    }
    .method-GET { background: rgba(56, 189, 248, 0.15); color: var(--blue); }
    .method-POST { background: rgba(34, 197, 94, 0.15); color: var(--green); }
    .method-PUT { background: rgba(168, 85, 247, 0.15); color: var(--purple); }
    .method-PATCH { background: rgba(234, 179, 8, 0.15); color: #eab308; }
    .method-DELETE { background: rgba(239, 68, 68, 0.15); color: var(--red); }
    code { font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace; font-size: 0.85rem; }
    pre {
      background: #020617;
      border: 1px solid var(--border);
      border-radius: 0.375rem;
      padding: 0.75rem;
      overflow-x: auto;
      font-size: 0.8rem;
      margin-top: 0.6rem;
      max-height: 520px;
    }
  </style>
</head>
<body>
  <header>
    <div class="brand">
      <h1>🔥 Flint FHIR R4 API &amp; SMART v2 Sandbox</h1>
      <span class="badge">FHIR R4 4.0.1</span>
      <span class="badge">google/fhir Proto3 BYTEA (0 JSONB)</span>
      <span class="badge">US Core $docref, $provenance &amp; $export</span>
      <span class="badge">50-Check Certification ($certify)</span>
    </div>
    <div class="controls" id="smart-token-minter">
      <input type="text" id="bearerToken" placeholder="SMART v2 Bearer JWT (or click Mint below)..." />
      <button class="btn btn-secondary" onclick="mintToken('system/*.cruds', '')">Mint System Admin Token</button>
      <button class="btn btn-secondary" onclick="mintToken('patient/*.rs', 'example-patient-1')">Mint Patient Token (example-patient-1)</button>
      <button class="btn btn-secondary" onclick="mintToken('patient/Observation.rs?category=vital-signs', 'example-patient-1')">Mint Granular Vital-Signs Token</button>
      <a class="btn" href="/fhir/r4/{{TENANT}}/openapi.json" target="_blank">OpenAPI 3.1 JSON</a>
      <a class="btn" href="/fhir/r4/{{TENANT}}/metadata" target="_blank">CapabilityStatement</a>
    </div>
  </header>
  <main>
    <h2 id="recipe-runner">1. 1-Click Clinical Recipe Runner</h2>
    <div class="recipes-grid" id="recipesGrid"></div>

    <h2>2. Interactive FHIR R4 Request Workbench &amp; Live Response Inspector</h2>
    <div class="workbench-panel" id="request-workbench">
      <div class="workbench-row">
        <select id="reqMethod">
          <option value="GET">GET</option>
          <option value="POST">POST</option>
          <option value="PUT">PUT</option>
          <option value="PATCH">PATCH</option>
          <option value="DELETE">DELETE</option>
        </select>
        <input type="text" id="reqPath" value="/Patient/example-patient-1/$summary" style="flex: 1; min-width: 320px;" />
        <input type="text" id="reqXProvenance" placeholder="Optional X-Provenance header (e.g. true)" style="min-width: 240px;" />
        <button class="btn btn-green" onclick="executeWorkbench()">Execute Request</button>
        <button class="btn btn-secondary" onclick="copyAsCurl()">Copy as curl</button>
      </div>
      <textarea id="reqBody" placeholder="Optional JSON Request Body (for POST / PUT / PATCH)..."></textarea>
      <pre id="liveOutput">Select a Clinical Recipe above or click "Execute Request" to inspect live FHIR R4 JSON / NDJSON output.</pre>
    </div>

    <h2>3. FHIR R4 / US Core / ONC (g)(10) Conformance Matrix</h2>
    <table>
      <thead>
        <tr>
          <th>Category</th>
          <th>Capability</th>
          <th>Specification</th>
          <th>Endpoint / Target</th>
          <th>Status</th>
        </tr>
      </thead>
      <tbody id="matrixBody"></tbody>
    </table>

    <h2>4. Interactive OpenAPI 3.1 Endpoint Catalog</h2>
    <div id="endpointsContainer">Loading OpenAPI 3.1 specification...</div>
  </main>
  <script>
    const tenant = "{{TENANT}}";
    const specUrl = "/fhir/r4/" + tenant + "/openapi.json";

    const clinicalRecipes = [
      {
        title: "1. International Patient Summary ($summary)",
        desc: "Assemble an HL7 IPS document Bundle (LOINC 60591-5) for example-patient-1.",
        method: "GET",
        path: "/Patient/example-patient-1/$summary",
        xProv: "",
        body: ""
      },
      {
        title: "2. US Core Clinical Notes ($docref)",
        desc: "Fetch stored clinical notes + on-demand base64 IPS DocumentReference for example-patient-1.",
        method: "GET",
        path: "/DocumentReference/$docref?patient=example-patient-1&on-demand=true",
        xProv: "",
        body: ""
      },
      {
        title: "3. Longitudinal Patient Compartment ($everything)",
        desc: "Retrieve Patient/example-patient-1 compartment filtered by _type with pagination links.",
        method: "GET",
        path: "/Patient/example-patient-1/$everything?_type=Patient,Condition,Observation&_count=10",
        xProv: "",
        body: ""
      },
      {
        title: "4. SDC Form Pre-Population ($populate)",
        desc: "Pre-populate a PHQ-9 QuestionnaireResponse from Patient/example-patient-1 demographics & vitals.",
        method: "POST",
        path: "/Questionnaire/$populate",
        xProv: "",
        body: JSON.stringify({
          resourceType: "Parameters",
          parameter: [
            { name: "subject", valueReference: { reference: "Patient/example-patient-1" } },
            {
              name: "questionnaire",
              resource: {
                resourceType: "Questionnaire",
                id: "sandbox-phq2",
                status: "active",
                item: [
                  { linkId: "pat-name", text: "Patient Name", type: "string" },
                  { linkId: "pat-dob", text: "Birth Date", type: "date" }
                ]
              }
            }
          ]
        }, null, 2)
      },
      {
        title: "5. SDC Clinical Extraction + Auto-Provenance ($extract)",
        desc: "Extract coded Observations from a QuestionnaireResponse and automatically generate US Core Provenance.",
        method: "POST",
        path: "/QuestionnaireResponse/$extract",
        xProv: "",
        body: JSON.stringify({
          resourceType: "QuestionnaireResponse",
          id: "qr-sandbox-1",
          status: "completed",
          subject: { reference: "Patient/example-patient-1" },
          item: [
            {
              linkId: "heart-rate",
              text: "Heart Rate",
              code: [{ system: "http://loinc.org", code: "8867-4", display: "Heart rate" }],
              answer: [{ valueQuantity: { value: 72, unit: "/min", system: "http://unitsofmeasure.org", code: "/min" } }]
            }
          ]
        }, null, 2)
      },
      {
        title: "6. Write Observation with X-Provenance Lineage",
        desc: "Create a US Core Vital Signs Observation with X-Provenance: true to auto-record Provenance.",
        method: "POST",
        path: "/Observation",
        xProv: "true",
        body: JSON.stringify({
          resourceType: "Observation",
          status: "final",
          category: [{ coding: [{ system: "http://terminology.hl7.org/CodeSystem/observation-category", code: "vital-signs" }] }],
          code: { coding: [{ system: "http://loinc.org", code: "8867-4", display: "Heart rate" }] },
          subject: { reference: "Patient/example-patient-1" },
          effectiveDateTime: "2025-02-01T10:00:00Z",
          valueQuantity: { value: 74, unit: "/min", system: "http://unitsofmeasure.org", code: "/min" }
        }, null, 2)
      },
      {
        title: "7. Da Vinci PAS Prior Authorization (Claim/$submit)",
        desc: "Submit a FHIR R4 Claim for real-time adjudication and persistence of ClaimResponse.",
        method: "POST",
        path: "/Claim/$submit",
        xProv: "",
        body: JSON.stringify({
          resourceType: "Parameters",
          parameter: [
            {
              name: "resource",
              resource: {
                resourceType: "Claim",
                status: "active",
                type: { coding: [{ system: "http://terminology.hl7.org/CodeSystem/claim-type", code: "professional" }] },
                use: "preauthorization",
                patient: { reference: "Patient/example-patient-1" },
                created: "2025-02-01",
                provider: { reference: "Practitioner/example-practitioner-1" },
                priority: { coding: [{ code: "normal" }] },
                insurance: [{ sequence: 1, focal: true, coverage: { reference: "Coverage/example-coverage-1" } }],
                total: { value: 250.0, currency: "USD" }
              }
            }
          ]
        }, null, 2)
      },
      {
        title: "8. CMS165 Blood Pressure eCQM ($evaluate-measure)",
        desc: "Evaluate CMS165 Controlling High Blood Pressure across the tenant cohort.",
        method: "GET",
        path: "/Measure/cms165-bp-control/$evaluate-measure?periodStart=2020-01-01&periodEnd=2030-12-31",
        xProv: "",
        body: ""
      },
      {
        title: "9. CDS Hooks v2.0 Patient View",
        desc: "Invoke the built-in flint-patient-view CDS Hook for example-patient-1.",
        method: "POST",
        path: "/cds-services/flint-patient-view",
        xProv: "",
        body: JSON.stringify({
          hook: "patient-view",
          hookInstance: "sandbox-hook-001",
          context: { userId: "Practitioner/example-practitioner-1", patientId: "example-patient-1" }
        }, null, 2)
      },
      {
        title: "10. 50-Check Automated Certification ($certify)",
        desc: "Execute the 50-check FHIR R4, US Core, CQM, CDS Hooks, SDC, IPS, Da Vinci, $docref & Provenance suite.",
        method: "GET",
        path: "/$certify",
        xProv: "",
        body: ""
      }
    ];

    function renderRecipes() {
      const grid = document.getElementById("recipesGrid");
      grid.innerHTML = clinicalRecipes.map((r, i) => ` + "`" + `
        <div class="recipe-card">
          <div>
            <div class="recipe-title">${r.title}</div>
            <div class="recipe-desc">${r.desc}</div>
          </div>
          <div style="display:flex; justify-content:space-between; align-items:center; margin-top:0.4rem;">
            <span class="method method-${r.method}">${r.method}</span>
            <button class="btn" onclick="loadAndRunRecipe(${i})">Load &amp; Run</button>
          </div>
        </div>
      ` + "`" + `).join("");
    }

    function loadAndRunRecipe(idx) {
      const r = clinicalRecipes[idx];
      document.getElementById("reqMethod").value = r.method;
      document.getElementById("reqPath").value = r.path;
      document.getElementById("reqXProvenance").value = r.xProv || "";
      document.getElementById("reqBody").value = r.body || "";
      executeWorkbench();
    }

    async function mintToken(scope, patient) {
      const out = document.getElementById("liveOutput");
      const form = new URLSearchParams();
      form.set("grant_type", "client_credentials");
      form.set("client_id", "flint-sandbox");
      form.set("scope", scope);
      if (patient) form.set("patient", patient);
      try {
        const resp = await fetch("/oauth2/token", {
          method: "POST",
          headers: { "Content-Type": "application/x-www-form-urlencoded" },
          body: form.toString()
        });
        const data = await resp.json();
        if (data.access_token) {
          document.getElementById("bearerToken").value = data.access_token;
          out.textContent = "Minted SMART v2 Bearer Token (scope: " + scope + (patient ? ", patient: " + patient : "") + ")\n\n" + JSON.stringify(data, null, 2);
        } else {
          out.textContent = "Token error:\n" + JSON.stringify(data, null, 2);
        }
      } catch (err) {
        out.textContent = "Token mint error: " + err;
      }
    }

    async function executeWorkbench() {
      const out = document.getElementById("liveOutput");
      const method = document.getElementById("reqMethod").value;
      const path = document.getElementById("reqPath").value.trim();
      const xProv = document.getElementById("reqXProvenance").value.trim();
      const body = document.getElementById("reqBody").value.trim();
      const token = document.getElementById("bearerToken").value.trim();

      const headers = {};
      if (token) headers["Authorization"] = "Bearer " + token;
      if (xProv) headers["X-Provenance"] = xProv;
      if (method === "POST" || method === "PUT" || method === "PATCH") {
        headers["Content-Type"] = method === "PATCH" && body.startsWith("[")
          ? "application/json-patch+json"
          : "application/fhir+json";
      }

      const fullUrl = path.startsWith("/fhir/r4") ? path : "/fhir/r4/" + tenant + (path === "/" ? "" : path);
      out.textContent = "Requesting " + method + " " + fullUrl + " ...";
      try {
        const opts = { method, headers };
        if ((method === "POST" || method === "PUT" || method === "PATCH") && body) {
          opts.body = body;
        }
        const resp = await fetch(fullUrl, opts);
        const text = await resp.text();
        let headerInfo = "HTTP " + resp.status + " (" + (resp.headers.get("Content-Type") || "") + ")";
        const loc = resp.headers.get("Location");
        const provLoc = resp.headers.get("X-Provenance-Location");
        if (loc) headerInfo += "\nLocation: " + loc;
        if (provLoc) headerInfo += "\nX-Provenance-Location: " + provLoc;
        out.textContent = headerInfo + "\n\n" + text;
      } catch (err) {
        out.textContent = "Error: " + err;
      }
    }

    function copyAsCurl() {
      const method = document.getElementById("reqMethod").value;
      const path = document.getElementById("reqPath").value.trim();
      const xProv = document.getElementById("reqXProvenance").value.trim();
      const body = document.getElementById("reqBody").value.trim();
      const token = document.getElementById("bearerToken").value.trim();
      const fullUrl = window.location.origin + (path.startsWith("/fhir/r4") ? path : "/fhir/r4/" + tenant + (path === "/" ? "" : path));

      let cmd = "curl -i -X " + method + " '" + fullUrl + "'";
      if (token) cmd += " \\\n  -H 'Authorization: Bearer " + token + "'";
      if (xProv) cmd += " \\\n  -H 'X-Provenance: " + xProv.replace(/'/g, "'\\''") + "'";
      if ((method === "POST" || method === "PUT" || method === "PATCH") && body) {
        cmd += " \\\n  -H 'Content-Type: application/fhir+json' \\\n  -d '" + body.replace(/'/g, "'\\''") + "'";
      }
      navigator.clipboard.writeText(cmd);
      document.getElementById("liveOutput").textContent = "Copied curl command to clipboard:\n\n" + cmd;
    }

    function runTryIt(path, method) {
      document.getElementById("reqMethod").value = method || "GET";
      document.getElementById("reqPath").value = path;
      executeWorkbench();
    }

    renderRecipes();

    fetch(specUrl)
      .then(r => r.json())
      .then(spec => {
        const matrix = spec["x-flint-conformance"] || [];
        const tbody = document.getElementById("matrixBody");
        tbody.innerHTML = matrix.map(row => ` + "`" + `
          <tr>
            <td><strong>${row.category}</strong></td>
            <td>${row.capability}</td>
            <td><code>${row.specification}</code></td>
            <td><code>${row.endpoint}</code></td>
            <td class="status-ok">✔ ${row.status}</td>
          </tr>
        ` + "`" + `).join("");

        const container = document.getElementById("endpointsContainer");
        const cards = [];
        for (const [path, methods] of Object.entries(spec.paths || {})) {
          for (const [method, details] of Object.entries(methods)) {
            const upper = method.toUpperCase();
            const tryBtn = (upper === "GET" && !path.includes("{"))
              ? ` + "`" + `<button class="btn" onclick="runTryIt('${path}', 'GET')">Try GET</button>` + "`" + `
              : ` + "`" + `<button class="btn btn-secondary" onclick="runTryIt('${path.replace("{id}", "example-patient-1")}', '${upper}')">Load in Workbench</button>` + "`" + `;
            cards.push(` + "`" + `
              <div class="endpoint-card">
                <div class="endpoint-header">
                  <div>
                    <span class="method method-${upper}">${upper}</span>
                    <code>/fhir/r4/${tenant}${path === "/" ? "" : path}</code>
                    <span style="color: var(--muted); margin-left: 0.5rem;">${details.summary || ""}</span>
                  </div>
                  <div>${tryBtn}</div>
                </div>
              </div>
            ` + "`" + `);
          }
        }
        container.innerHTML = cards.join("");
      });
  </script>
</body>
</html>
`
