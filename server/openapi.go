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
			{"name": "race", "type": "token"},
			{"name": "ethnicity", "type": "token"},
			{"name": "birthsex", "type": "token"},
			{"name": "marital-status", "type": "token"},
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
		{"name": "_filter", "type": "special"},
		{"name": "_content", "type": "string"},
		{"name": "_text", "type": "string"},
	}
	for rt, sp := range defs {
		defs[rt] = mergeCustomSearchParams(rt, append(sp, metaParams...))
	}
	return defs
}

func mergeCustomSearchParams(rt string, sp []map[string]string) []map[string]string {
	custom := postgres.CustomSearchParamsForType(rt)
	if len(custom) == 0 {
		return sp
	}
	seen := make(map[string]bool, len(sp))
	out := make([]map[string]string, 0, len(sp)+len(custom))
	for _, entry := range sp {
		seen[entry["name"]] = true
		out = append(out, entry)
	}
	for _, c := range custom {
		if seen[c.Code] {
			continue
		}
		seen[c.Code] = true
		item := map[string]string{
			"name": c.Code,
			"type": c.Type,
		}
		if c.URL != "" {
			item["definition"] = c.URL
		}
		out = append(out, item)
	}
	return out
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
					{"name": "_until", "in": "query", "description": "Only include resources updated at or before this ISO-8601 timestamp", "schema": map[string]any{"type": "string"}},
					{"name": "_elements", "in": "query", "description": "Comma-separated FHIR elements or ResourceType.element tokens to project (adds SUBSETTED tag)", "schema": map[string]any{"type": "string"}},
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
					{"name": "_until", "in": "query", "description": "ISO-8601 timestamp upper bound", "schema": map[string]any{"type": "string"}},
					{"name": "start", "in": "query", "description": "Clinical care-date lower bound (FHIR R4 $everything start)", "schema": map[string]any{"type": "string", "format": "date"}},
					{"name": "end", "in": "query", "description": "Clinical care-date upper bound (FHIR R4 $everything end)", "schema": map[string]any{"type": "string", "format": "date"}},
					{"name": "_elements", "in": "query", "description": "Comma-separated FHIR elements or ResourceType.element tokens to project", "schema": map[string]any{"type": "string"}},
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
					{"name": "_until", "in": "query", "description": "ISO-8601 timestamp upper bound", "schema": map[string]any{"type": "string"}},
					{"name": "start", "in": "query", "description": "Clinical care-date lower bound (FHIR R4 $everything start)", "schema": map[string]any{"type": "string", "format": "date"}},
					{"name": "end", "in": "query", "description": "Clinical care-date upper bound (FHIR R4 $everything end)", "schema": map[string]any{"type": "string", "format": "date"}},
					{"name": "_elements", "in": "query", "description": "Comma-separated FHIR elements or ResourceType.element tokens to project", "schema": map[string]any{"type": "string"}},
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
					{"name": "_until", "in": "query", "description": "ISO-8601 timestamp upper bound", "schema": map[string]any{"type": "string"}},
					{"name": "start", "in": "query", "description": "Clinical care-date lower bound (FHIR R4 $everything start)", "schema": map[string]any{"type": "string", "format": "date"}},
					{"name": "end", "in": "query", "description": "Clinical care-date upper bound (FHIR R4 $everything end)", "schema": map[string]any{"type": "string", "format": "date"}},
					{"name": "_elements", "in": "query", "description": "Comma-separated FHIR elements or ResourceType.element tokens to project", "schema": map[string]any{"type": "string"}},
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
		"/$reindex": map[string]any{
			"post": map[string]any{
				"tags":        []string{"System Operations"},
				"summary":     "Rebuild spidx_* covering search indexes across all active tenant resources ($reindex)",
				"operationId": "systemReindex",
				"responses": map[string]any{
					"200": map[string]any{"description": "OperationOutcome summarizing reindexed resources"},
				},
			},
		},
		"/PlanDefinition/$apply": map[string]any{
			"get": map[string]any{
				"tags":        []string{"PlanDefinition"},
				"summary":     "Apply a clinical PlanDefinition protocol to a Patient and synthesize a CarePlan ($apply)",
				"operationId": "planDefinitionApplyGet",
				"parameters": []map[string]any{
					{"name": "planDefinition", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "subject", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "encounter", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "practitioner", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "organization", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Personalized FHIR R4 CarePlan (or Bundle)"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"PlanDefinition"},
				"summary":     "Apply a clinical PlanDefinition protocol to a Patient via POST Parameters ($apply)",
				"operationId": "planDefinitionApplyPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Personalized FHIR R4 CarePlan (or Bundle)"},
				},
			},
		},
		"/PlanDefinition/{id}/$apply": map[string]any{
			"get": map[string]any{
				"tags":        []string{"PlanDefinition"},
				"summary":     "Apply PlanDefinition/{id} to a Patient and synthesize a CarePlan ($apply)",
				"operationId": "planDefinitionApplyByIdGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "subject", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Personalized FHIR R4 CarePlan (or Bundle)"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"PlanDefinition"},
				"summary":     "Apply PlanDefinition/{id} to a Patient ($apply, POST Parameters)",
				"operationId": "planDefinitionApplyByIdPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Personalized FHIR R4 CarePlan (or Bundle)"},
				},
			},
		},
		"/ActivityDefinition/$apply": map[string]any{
			"get": map[string]any{
				"tags":        []string{"ActivityDefinition"},
				"summary":     "Apply a clinical ActivityDefinition to a Patient and synthesize a ServiceRequest or MedicationRequest ($apply)",
				"operationId": "activityDefinitionApplyGet",
				"parameters": []map[string]any{
					{"name": "activityDefinition", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "subject", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Synthesized FHIR R4 ServiceRequest or MedicationRequest"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"ActivityDefinition"},
				"summary":     "Apply a clinical ActivityDefinition to a Patient ($apply, POST Parameters)",
				"operationId": "activityDefinitionApplyPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Synthesized FHIR R4 ServiceRequest or MedicationRequest"},
				},
			},
		},
		"/ActivityDefinition/{id}/$apply": map[string]any{
			"get": map[string]any{
				"tags":        []string{"ActivityDefinition"},
				"summary":     "Apply ActivityDefinition/{id} to a Patient ($apply)",
				"operationId": "activityDefinitionApplyByIdGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "subject", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Synthesized FHIR R4 ServiceRequest or MedicationRequest"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"ActivityDefinition"},
				"summary":     "Apply ActivityDefinition/{id} to a Patient ($apply, POST Parameters)",
				"operationId": "activityDefinitionApplyByIdPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Synthesized FHIR R4 ServiceRequest or MedicationRequest"},
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
		"/Patient/$bulk-member-match": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Patient"},
				"summary":     "HL7 Da Vinci PDex v2.0.0 Patient/$bulk-member-match multi-member cohort matching and Group persistence",
				"operationId": "patientBulkMemberMatch",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing MatchedMembers Group reference, MatchedGroup, and NonMatchedMembers"},
				},
			},
		},
		"/Group/{id}/$davinci-data-export": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Group"},
				"summary":     "HL7 Da Vinci PDex v2.0.0 Group/{id}/$davinci-data-export payer-to-payer clinical cohort Bundle export",
				"operationId": "groupDaVinciDataExportGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: collection) of US Core clinical resources for matched Group members"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Group"},
				"summary":     "HL7 Da Vinci PDex v2.0.0 Group/{id}/$davinci-data-export (POST)",
				"operationId": "groupDaVinciDataExportPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: collection) of US Core clinical resources for matched Group members"},
				},
			},
		},
		"/Encounter/$admit": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Encounter"},
				"summary":     "HL7 FHIR R4 Encounter/$admit inpatient admission workflow",
				"operationId": "encounterAdmit",
				"responses": map[string]any{
					"200": map[string]any{"description": "Persisted active inpatient Encounter resource"},
				},
			},
		},
		"/Encounter/{id}/$transfer": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Encounter"},
				"summary":     "HL7 FHIR R4 Encounter/{id}/$transfer inpatient bed/location transfer workflow",
				"operationId": "encounterTransfer",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Updated Encounter resource with location transfer history"},
				},
			},
		},
		"/Encounter/{id}/$discharge": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Encounter"},
				"summary":     "HL7 FHIR R4 Encounter/{id}/$discharge inpatient discharge & LOINC 18842-5 Discharge Summary Composition synthesis",
				"operationId": "encounterDischarge",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing finished Encounter and LOINC 18842-5 Discharge Summary Composition"},
				},
			},
		},
		"/MedicationRequest/{id}/$dispense": map[string]any{
			"post": map[string]any{
				"tags":        []string{"MedicationRequest"},
				"summary":     "Fulfill a MedicationRequest by synthesizing and persisting a US Core MedicationDispense ($dispense)",
				"operationId": "medicationRequestDispense",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Persisted US Core MedicationDispense resource"},
				},
			},
		},
		"/PractitionerRole/$find-providers": map[string]any{
			"get": map[string]any{
				"tags":        []string{"PractitionerRole"},
				"summary":     "HL7 Da Vinci PDex Plan-Net v1.1.0 PractitionerRole/$find-providers provider directory search",
				"operationId": "practitionerRoleFindProvidersGet",
				"parameters": []map[string]any{
					{"name": "specialty", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "organization", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "location", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: searchset) of Plan-Net PractitionerRole and included Practitioner/Organization/Location entries"},
				},
			},
		},
		"/HealthcareService/$find-services": map[string]any{
			"get": map[string]any{
				"tags":        []string{"HealthcareService"},
				"summary":     "HL7 Da Vinci PDex Plan-Net v1.1.0 HealthcareService/$find-services directory search",
				"operationId": "healthcareServiceFindServicesGet",
				"parameters": []map[string]any{
					{"name": "category", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "specialty", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: searchset) of Plan-Net HealthcareService entries"},
				},
			},
		},
		"/ServiceRequest/$sdoh-referral": map[string]any{
			"post": map[string]any{
				"tags":        []string{"ServiceRequest"},
				"summary":     "HL7 Gravity SDOH Clinical Care v2.1.0 ServiceRequest/$sdoh-referral closed-loop community referral & Task creation",
				"operationId": "serviceRequestSDOHReferral",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing SDOHCC-ServiceRequest and SDOHCC-TaskForReferralManagement"},
				},
			},
		},
		"/Patient/{id}/$sdoh-summary": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient"},
				"summary":     "HL7 Gravity SDOH Clinical Care v2.1.0 Patient/{id}/$sdoh-summary social determinants summary Bundle",
				"operationId": "patientSDOHSummaryGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: collection) of SDOH clinical screenings, health concerns, goals, and referrals"},
				},
			},
		},
		"/Consent/$evaluate": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Consent"},
				"summary":     "HL7 FHIR R4 Consent/$evaluate privacy directive & confidentiality security label evaluation",
				"operationId": "consentEvaluateGet",
				"parameters": []map[string]any{
					{"name": "patient", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "actor", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "purpose", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing decision (permit|deny), securityLabel, and matchedConsent"},
				},
			},
		},
		"/Patient/{id}/$carin-bb": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient"},
				"summary":     "HL7 CARIN Blue Button v2.0.0 Patient/{id}/$carin-bb Consumer-Directed Payer Data Exchange Bundle",
				"operationId": "patientCarinBBGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "C4BB Bundle (type: searchset) of Patient, Coverage, and ExplanationOfBenefit resources"},
				},
			},
		},
		"/ExplanationOfBenefit/$carin-bb-summary": map[string]any{
			"get": map[string]any{
				"tags":        []string{"ExplanationOfBenefit"},
				"summary":     "HL7 CARIN Blue Button v2.0.0 ExplanationOfBenefit/$carin-bb-summary financial benefit utilization summary",
				"operationId": "eobCarinBBSummaryGet",
				"parameters": []map[string]any{
					{"name": "patient", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing claimCount, totalSubmitted, totalBenefitPaid, totalMemberLiability, and latestExplanationOfBenefit"},
				},
			},
		},
		"/Encounter/{id}/$ecr-report": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Encounter"},
				"summary":     "HL7 Electronic Case Reporting (eCR v2.1.1) Encounter/{id}/$ecr-report eICR & Reportability Response generation",
				"operationId": "encounterECRReportPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing eicrBundle (LOINC 55751-2) and reportabilityResponse Communication (LOINC 88085-6)"},
				},
			},
		},
		"/PlanDefinition/$ersd-lookup": map[string]any{
			"get": map[string]any{
				"tags":        []string{"PlanDefinition"},
				"summary":     "HL7 eCR v2.1.1 PlanDefinition/$ersd-lookup Electronic Reporting and Surveillance Distribution (eRSD) RCTC trigger lookup",
				"operationId": "planDefinitionERSDLookupGet",
				"responses": map[string]any{
					"200": map[string]any{"description": "US Public Health eRSD PlanDefinition resource"},
				},
			},
		},
		"/Patient/{id}/$decease": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Patient"},
				"summary":     "HL7 Vital Records Death Reporting (VRDR v2.2.0) Patient/{id}/$decease death registration & certificate synthesis",
				"operationId": "patientVRDRDeceasePost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing updated Patient, VRDR Death Certificate Composition (LOINC 64297-5), and Cause of Death Observation (LOINC 69453-9)"},
				},
			},
		},
		"/Patient/{id}/$mcode-summary": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient"},
				"summary":     "HL7 mCODE v3.0.0 Patient/{id}/$mcode-summary Oncology Patient Summary Bundle",
				"operationId": "patientMCODESummaryGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "mCODE Patient Bundle (collection) containing mcode-cancer-patient, mcode-primary-cancer-condition, and mcode-tnm-clinical-stage-group resources"},
				},
			},
		},
		"/Condition/{id}/$stage-cancer": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Condition"},
				"summary":     "HL7 mCODE v3.0.0 Condition/{id}/$stage-cancer AJCC TNM clinical cancer staging operation",
				"operationId": "conditionStageCancerPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing staged Condition and mcode-tnm-clinical-stage-group Observation (LOINC 21908-9)"},
				},
			},
		},
		"/MedicationKnowledge/$formulary-lookup": map[string]any{
			"get": map[string]any{
				"tags":        []string{"MedicationKnowledge"},
				"summary":     "HL7 Da Vinci PDex US Drug Formulary v2.0.1 MedicationKnowledge/$formulary-lookup drug tier & copay lookup",
				"operationId": "medicationKnowledgeFormularyLookupGet",
				"parameters": []map[string]any{
					{"name": "code", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "planId", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing drugTier, priorAuthorization, stepTherapyLimit, quantityLimit, copay, and usdf-FormularyDrug"},
				},
			},
		},
		"/Coverage/{id}/$formulary-coverage": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Coverage"},
				"summary":     "HL7 Da Vinci PDex US Drug Formulary v2.0.1 Coverage/{id}/$formulary-coverage member benefit evaluation",
				"operationId": "coverageFormularyCoverageGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "code", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing covered status, drugTier, copay, and utilization management flags"},
				},
			},
		},
		"/Bundle/$iti-65": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Bundle"},
				"summary":     "IHE MHD v4.2.1 ITI-65 Provide Document Bundle transaction",
				"operationId": "bundleIHEMHDITI65Post",
				"responses": map[string]any{
					"200": map[string]any{"description": "IHE.MHD.Minimal.ProvideBundleResponse transaction-response Bundle"},
				},
			},
		},
		"/DocumentReference/$iti-67": map[string]any{
			"get": map[string]any{
				"tags":        []string{"DocumentReference"},
				"summary":     "IHE MHD v4.2.1 ITI-67 Find Document References query",
				"operationId": "documentReferenceIHEMHDITI67Get",
				"parameters": []map[string]any{
					{"name": "patient", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "status", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "IHE.MHD.FindDocumentReferencesComprehensiveResponseMessage searchset Bundle"},
				},
			},
		},
		"/Patient/{id}/$find-subject-variants": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient"},
				"summary":     "HL7 Clinical Genomics v3.0.0 Patient/{id}/$find-subject-variants genomic variant discovery",
				"operationId": "patientFindSubjectVariantsGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "genes", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing presence (boolean) and variants (LOINC 69548-6)"},
				},
			},
		},
		"/Patient/{id}/$find-subject-tx-implications": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient"},
				"summary":     "HL7 Clinical Genomics v3.0.0 Patient/{id}/$find-subject-tx-implications pharmacogenomic & oncology therapeutic implications",
				"operationId": "patientFindSubjectTxImplicationsGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "treatments", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing implications (therapeutic-implication Observation, LOINC 51963-7)"},
				},
			},
		},
		"/Patient/{id}/$pacio-summary": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient"},
				"summary":     "HL7 PACIO v2.0.0 Patient/{id}/$pacio-summary Functional & Cognitive Status Bundle",
				"operationId": "patientPACIOSummaryGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "PACIO Bundle (pacio-fs-bundle) containing Section GG functional mobility (83254-3) and BIMS cognitive status (52491-8) Observations"},
				},
			},
		},
		"/QuestionnaireResponse/{id}/$pacio-assess": map[string]any{
			"post": map[string]any{
				"tags":        []string{"QuestionnaireResponse"},
				"summary":     "HL7 PACIO v2.0.0 QuestionnaireResponse/{id}/$pacio-assess post-acute functional & BIMS cognitive assessment",
				"operationId": "questionnaireResponsePACIOAssessPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing bimsScore, functionalObservation, and cognitiveObservation"},
				},
			},
		},
		"/Encounter/{id}/$nhsn-hai-report": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Encounter"},
				"summary":     "CDC NHSN HAI / dQM v1.1.0 Encounter/{id}/$nhsn-hai-report Healthcare-Associated Infection & AUR reporting",
				"operationId": "encounterNHSNHAIReportPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing haiDetected and NHSN DEQM MeasureReport"},
				},
			},
		},
		"/Measure/$nhsn-acute-care": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Measure"},
				"summary":     "CDC NHSN v1.1.0 Measure/$nhsn-acute-care Acute Care Hospital HAI & AUR dQM Measure",
				"operationId": "measureNHSNAcuteCareGet",
				"responses": map[string]any{
					"200": map[string]any{"description": "CDC NHSN HAI & AUR Measure resource (nhsn-hai-aur-measure)"},
				},
			},
		},
		"/ResearchStudy/{id}/$rwd-cohort": map[string]any{
			"get": map[string]any{
				"tags":        []string{"ResearchStudy", "Group"},
				"summary":     "HL7 Vulcan Real-World Data v1.0.0 ResearchStudy/{id}/$rwd-cohort EHR clinical trial cohort extraction",
				"operationId": "researchStudyRWDCohortGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing matchedCount, researchStudy, and Vulcan RWD cohortGroup (Group)"},
				},
			},
		},
		"/ResearchSubject/{id}/$enroll": map[string]any{
			"post": map[string]any{
				"tags":        []string{"ResearchSubject", "CarePlan"},
				"summary":     "HL7 Vulcan Schedule of Activities v1.0.0 ResearchSubject/{id}/$enroll subject enrollment & SoA CarePlan synthesis",
				"operationId": "researchSubjectEnrollPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing enrolled ResearchSubject (status: on-study) and Vulcan Schedule of Activities CarePlan"},
				},
			},
		},
		"/Patient/{id}/$birth-report": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Patient", "Composition", "Observation"},
				"summary":     "HL7 Vital Records Birth and Fetal Death Reporting (BFDR v1.1.0) Patient/{id}/$birth-report live birth certificate & neonatal observations",
				"operationId": "patientBirthReportPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing newborn Patient, birthWeight Observation (8339-4), apgarScore Observation (9274-2), and liveBirthCertificate Composition (68998-4)"},
				},
			},
		},
		"/Composition/$bfdr-summary": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Composition", "Bundle"},
				"summary":     "HL7 BFDR v1.1.0 Composition/$bfdr-summary Live Birth Certificate Document Bundle",
				"operationId": "compositionBFDRSummaryGet",
				"parameters": []map[string]any{
					{"name": "patient", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "BFDR Live Birth Document Bundle (Bundle-document-live-birth-report, type: document)"},
				},
			},
		},
		"/Patient/$ihe-pdqm": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient"},
				"summary":     "IHE PDQm v3.0.0 ITI-78 Mobile Patient Demographics Query",
				"operationId": "patientIHEPDQmGet",
				"parameters": []map[string]any{
					{"name": "family", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "given", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "gender", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "IHE.PDQm.QueryPatientResourceResponseMessage searchset Bundle"},
				},
			},
		},
		"/DocumentReference/{id}/$ihe-iti-68": map[string]any{
			"get": map[string]any{
				"tags":        []string{"DocumentReference", "Binary"},
				"summary":     "IHE MHD v4.2.1 ITI-68 Retrieve Document operation",
				"operationId": "documentReferenceIHEITI68Get",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing contentType, DocumentReference, and Binary document payload"},
				},
			},
		},
		"/Endpoint/$fast-endpoint-query": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Endpoint"},
				"summary":     "HL7 FAST National Healthcare Directory (NDH v1.0.0) Endpoint/$fast-endpoint-query discovery",
				"operationId": "endpointFASTEndpointQueryGet",
				"parameters": []map[string]any{
					{"name": "connectionType", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of ndh-Endpoint resources"},
				},
			},
		},
		"/Organization/{id}/$fast-verify": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Organization", "VerificationResult"},
				"summary":     "HL7 FAST National Healthcare Directory (NDH v1.0.0) Organization/{id}/$fast-verify attestation & primary source verification",
				"operationId": "organizationFASTVerifyPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing verified (boolean), Organization, and ndh-VerificationResult (status: attested)"},
				},
			},
		},
		"/Patient/{id}/$physical-activity-summary": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient", "Observation", "Goal"},
				"summary":     "HL7 Physical Activity v1.0.0 Patient/{id}/$physical-activity-summary Exercise Vital Sign (EVS) & Goal Bundle",
				"operationId": "patientPhysicalActivitySummaryGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Collection Bundle containing Patient, EVS Observations (89555-7, 68516-4, 82290-8), and physical activity Goal"},
				},
			},
		},
		"/ServiceRequest/$physical-activity-referral": map[string]any{
			"post": map[string]any{
				"tags":        []string{"ServiceRequest", "Task"},
				"summary":     "HL7 Physical Activity v1.0.0 ServiceRequest/$physical-activity-referral closed-loop exercise referral & Task synthesis",
				"operationId": "serviceRequestPhysicalActivityReferralPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing pa-servicerequest (SNOMED 390893007) and pa-task-for-referral-management"},
				},
			},
		},
		"/MedicinalProduct/$epi-document": map[string]any{
			"get": map[string]any{
				"tags":        []string{"MedicinalProduct", "Composition", "Bundle"},
				"summary":     "HL7 Vulcan Electronic Medicinal Product Information (ePI v1.0.0) MedicinalProduct/$epi-document package leaflet Document Bundle",
				"operationId": "medicinalProductEPIDocumentGet",
				"parameters": []map[string]any{
					{"name": "name", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "ePI Document Bundle (Bundle-uv-epi, type: document) with SPL Composition (69719-3) and MedicinalProduct"},
				},
			},
		},
		"/MedicinalProductAuthorization/$evaluate-authorization": map[string]any{
			"post": map[string]any{
				"tags":        []string{"MedicinalProductAuthorization"},
				"summary":     "HL7 Vulcan ePI / BR&R v1.0.0 MedicinalProductAuthorization/$evaluate-authorization regulatory marketing authorization check",
				"operationId": "medicinalProductAuthorizationEvaluatePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing authorized (boolean), jurisdiction, and RegulatedAuthorization-uv-epi"},
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
					{"name": "_until", "in": "query", "description": "ISO-8601 timestamp upper bound", "schema": map[string]any{"type": "string"}},
					{"name": "start", "in": "query", "description": "Clinical care-date lower bound (FHIR R4 $everything start)", "schema": map[string]any{"type": "string", "format": "date"}},
					{"name": "end", "in": "query", "description": "Clinical care-date upper bound (FHIR R4 $everything end)", "schema": map[string]any{"type": "string", "format": "date"}},
					{"name": "_elements", "in": "query", "description": "Comma-separated FHIR elements or ResourceType.element tokens to project", "schema": map[string]any{"type": "string"}},
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
					{"name": "_until", "in": "query", "description": "ISO-8601 timestamp upper bound", "schema": map[string]any{"type": "string"}},
					{"name": "start", "in": "query", "description": "Clinical care-date lower bound (FHIR R4 $everything start)", "schema": map[string]any{"type": "string", "format": "date"}},
					{"name": "end", "in": "query", "description": "Clinical care-date upper bound (FHIR R4 $everything end)", "schema": map[string]any{"type": "string", "format": "date"}},
					{"name": "_elements", "in": "query", "description": "Comma-separated FHIR elements or ResourceType.element tokens to project", "schema": map[string]any{"type": "string"}},
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
		"/Group/{id}/$everything": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Group"},
				"summary":     "Assemble full Group cohort longitudinal Bundle across all active Patient members ($everything)",
				"operationId": "groupEverything",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "_type", "in": "query", "description": "Comma-separated resource types", "schema": map[string]any{"type": "string"}},
					{"name": "_since", "in": "query", "description": "ISO-8601 timestamp lower bound", "schema": map[string]any{"type": "string"}},
					{"name": "_until", "in": "query", "description": "ISO-8601 timestamp upper bound", "schema": map[string]any{"type": "string"}},
					{"name": "start", "in": "query", "description": "Clinical care-date lower bound (FHIR R4 $everything start)", "schema": map[string]any{"type": "string", "format": "date"}},
					{"name": "end", "in": "query", "description": "Clinical care-date upper bound (FHIR R4 $everything end)", "schema": map[string]any{"type": "string", "format": "date"}},
					{"name": "_elements", "in": "query", "description": "Comma-separated FHIR elements or ResourceType.element tokens to project", "schema": map[string]any{"type": "string"}},
					{"name": "_typeFilter", "in": "query", "description": "Comma-separated FHIR search criteria filters", "schema": map[string]any{"type": "string"}},
					{"name": "_count", "in": "query", "schema": map[string]any{"type": "integer"}},
					{"name": "_offset", "in": "query", "schema": map[string]any{"type": "integer"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: searchset) containing Group, active cohort Patients, and compartment resources"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Group"},
				"summary":     "Assemble full Group cohort longitudinal Bundle ($everything, POST Parameters)",
				"operationId": "groupEverythingPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: searchset) containing Group, active cohort Patients, and compartment resources"},
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
		"/$closure": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Terminology"},
				"summary":     "FHIR R4 Terminology Subsumption Closure Table Maintenance ($closure)",
				"operationId": "conceptMapClosure",
				"responses": map[string]any{
					"200": map[string]any{"description": "ConceptMap containing incremental or resynchronized subsumption relations"},
				},
			},
		},
		"/$data-requirements": map[string]any{
			"get": map[string]any{
				"tags":        []string{"ClinicalReasoning"},
				"summary":     "HL7 Clinical Reasoning $data-requirements operation returning a module-definition Library",
				"operationId": "dataRequirementsGet",
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR R4 Library (type: module-definition) with dataRequirement entries"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"ClinicalReasoning"},
				"summary":     "HL7 Clinical Reasoning $data-requirements operation (POST Parameters)",
				"operationId": "dataRequirementsPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR R4 Library (type: module-definition) with dataRequirement entries"},
				},
			},
		},
		"/$fhirpath": map[string]any{
			"get": map[string]any{
				"tags":        []string{"ClinicalReasoning"},
				"summary":     "Evaluate a FHIRPath R4 expression against a FHIR resource ($fhirpath)",
				"operationId": "fhirpathEvalGet",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing evaluated FHIRPath results"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"ClinicalReasoning"},
				"summary":     "Evaluate a FHIRPath R4 expression against an inline or stored FHIR resource ($fhirpath)",
				"operationId": "fhirpathEvalPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing evaluated FHIRPath results"},
				},
			},
		},
		"/Subscription/{id}/$events": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Subscription"},
				"summary":     "HL7 Subscriptions R5 Backport Subscription/{id}/$events notification replay",
				"operationId": "subscriptionEventsGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "eventsSinceNumber", "in": "query", "required": false, "schema": map[string]any{"type": "integer"}},
					{"name": "eventsUntilNumber", "in": "query", "required": false, "schema": map[string]any{"type": "integer"}},
					{"name": "content", "in": "query", "required": false, "schema": map[string]any{"type": "string", "enum": []string{"empty", "id-only", "full-resource"}}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: history) starting with SubscriptionStatus Parameters (event-notification)"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Subscription"},
				"summary":     "HL7 Subscriptions R5 Backport Subscription/{id}/$events notification replay (POST Parameters)",
				"operationId": "subscriptionEventsPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: history) starting with SubscriptionStatus Parameters (event-notification)"},
				},
			},
		},
		"/Measure/$collect-data": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Measure"},
				"summary":     "HL7 Da Vinci DEQM / Clinical Reasoning Measure/$collect-data operation",
				"operationId": "measureCollectDataGet",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing data-collection MeasureReport and clinical resources"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Measure"},
				"summary":     "HL7 Da Vinci DEQM / Clinical Reasoning Measure/$collect-data operation (POST Parameters)",
				"operationId": "measureCollectDataPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing data-collection MeasureReport and clinical resources"},
				},
			},
		},
		"/Measure/$submit-data": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Measure"},
				"summary":     "HL7 Da Vinci DEQM / Clinical Reasoning Measure/$submit-data operation persisting MeasureReport and clinical resources",
				"operationId": "measureSubmitDataPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: transaction-response)"},
				},
			},
		},
		"/Group/{id}/$add": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Group"},
				"summary":     "Atomically add member references to Group/{id} cohort ($add)",
				"operationId": "groupAddPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Updated FHIR R4 Group resource"},
				},
			},
		},
		"/Group/{id}/$remove": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Group"},
				"summary":     "Atomically remove member references from Group/{id} cohort ($remove)",
				"operationId": "groupRemovePost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Updated FHIR R4 Group resource"},
				},
			},
		},
		"/Observation/$lastn": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Observation"},
				"summary":     "HL7 FHIR R4 / US Core Observation/$lastn operation returning the most recent N observations per distinct code",
				"operationId": "observationLastNGet",
				"parameters": []map[string]any{
					{"name": "patient", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "subject", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "category", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "code", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "max", "in": "query", "required": false, "description": "Maximum observations per distinct code (default 1)", "schema": map[string]any{"type": "integer"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: searchset) of latest N Observations per distinct code"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Observation"},
				"summary":     "HL7 FHIR R4 / US Core Observation/$lastn operation (POST Parameters)",
				"operationId": "observationLastNPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: searchset) of latest N Observations per distinct code"},
				},
			},
		},
		"/Observation/$stats": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Observation"},
				"summary":     "HL7 FHIR R4 Observation/$stats operation computing clinical summary statistics (average, max, min, count, median, std-dev, sum, variance)",
				"operationId": "observationStatsGet",
				"parameters": []map[string]any{
					{"name": "subject", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "code", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "statistic", "in": "query", "required": true, "description": "One or more statistic codes (average, max, min, count, totalcount, median, std-dev, sum, variance)", "schema": map[string]any{"type": "string"}},
					{"name": "duration", "in": "query", "required": false, "schema": map[string]any{"type": "number"}},
					{"name": "start", "in": "query", "required": false, "schema": map[string]any{"type": "string", "format": "date"}},
					{"name": "end", "in": "query", "required": false, "schema": map[string]any{"type": "string", "format": "date"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing synthesized statistic Observation resources"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Observation"},
				"summary":     "HL7 FHIR R4 Observation/$stats operation (POST Parameters)",
				"operationId": "observationStatsPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing synthesized statistic Observation resources"},
				},
			},
		},
		"/Patient/$merge": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Patient"},
				"summary":     "FHIR R4 Master Patient Index survivorship & clinical compartment re-linking (Patient/$merge)",
				"operationId": "patientMergePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing outcome OperationOutcome, surviving Patient result, and completed merge Task"},
					"422": map[string]any{"description": "Invalid or identical source/target patients"},
				},
			},
		},
		"/Appointment/$find": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Appointment"},
				"summary":     "HL7 FHIR R4 / Argonaut Scheduling Appointment/$find searching free Slots and synthesizing proposed Appointments",
				"operationId": "appointmentFindGet",
				"parameters": []map[string]any{
					{"name": "start", "in": "query", "required": false, "schema": map[string]any{"type": "string", "format": "date-time"}},
					{"name": "end", "in": "query", "required": false, "schema": map[string]any{"type": "string", "format": "date-time"}},
					{"name": "specialty", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "visit-type", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "practitioner", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "location", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: searchset) of proposed Appointments and free Slots"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Appointment"},
				"summary":     "HL7 FHIR R4 / Argonaut Scheduling Appointment/$find (POST Parameters)",
				"operationId": "appointmentFindPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: searchset) of proposed Appointments and free Slots"},
				},
			},
		},
		"/Appointment/$hold": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Appointment"},
				"summary":     "HL7 FHIR R4 / Argonaut Scheduling Appointment/$hold tentatively reserving Slot(s) (status: pending / busy-tentative)",
				"operationId": "appointmentHoldPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle containing pending Appointment and OperationOutcome"},
					"422": map[string]any{"description": "Slot conflict (busy)"},
				},
			},
		},
		"/Appointment/$book": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Appointment"},
				"summary":     "HL7 FHIR R4 / Argonaut Scheduling Appointment/$book atomically booking Slot(s) (status: booked / busy)",
				"operationId": "appointmentBookPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle containing booked Appointment and OperationOutcome"},
					"422": map[string]any{"description": "Slot conflict (busy)"},
				},
			},
		},
		"/CapabilityStatement/$subset": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Discovery & Docs"},
				"summary":     "HL7 FHIR R4 CapabilityStatement/$subset returning a pruned CapabilityStatement for requested resource types",
				"operationId": "capabilityStatementSubsetGet",
				"parameters": []map[string]any{
					{"name": "resource", "in": "query", "required": false, "description": "Comma-separated or repeated FHIR resource type names", "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Subsetted FHIR R4 CapabilityStatement"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Discovery & Docs"},
				"summary":     "HL7 FHIR R4 CapabilityStatement/$subset (POST Parameters)",
				"operationId": "capabilityStatementSubsetPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Subsetted FHIR R4 CapabilityStatement"},
				},
			},
		},
		"/CapabilityStatement/$implements": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Discovery & Docs"},
				"summary":     "HL7 FHIR R4 CapabilityStatement/$implements conformance test verifying server satisfaction of a client/IG CapabilityStatement",
				"operationId": "capabilityStatementImplementsGet",
				"parameters": []map[string]any{
					{"name": "canonical", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing result (boolean) and issues (OperationOutcome)"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Discovery & Docs"},
				"summary":     "HL7 FHIR R4 CapabilityStatement/$implements conformance test (POST Parameters or CapabilityStatement)",
				"operationId": "capabilityStatementImplementsPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing result (boolean) and issues (OperationOutcome)"},
				},
			},
		},
		"/StructureDefinition/$snapshot": map[string]any{
			"get": map[string]any{
				"tags":        []string{"StructureDefinition"},
				"summary":     "HL7 FHIR R4 StructureDefinition/$snapshot generating a complete snapshot.element table from Protobuf descriptors and differential rules",
				"operationId": "structureDefinitionSnapshotGet",
				"parameters": []map[string]any{
					{"name": "url", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "StructureDefinition with populated snapshot.element"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"StructureDefinition"},
				"summary":     "HL7 FHIR R4 StructureDefinition/$snapshot (POST StructureDefinition or Parameters)",
				"operationId": "structureDefinitionSnapshotPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "StructureDefinition with populated snapshot.element"},
				},
			},
		},
		"/$process-message": map[string]any{
			"post": map[string]any{
				"tags":        []string{"MessageHeader"},
				"summary":     "HL7 FHIR R4 Messaging Framework $process-message ingesting a message Bundle (MessageHeader + focus resources) and returning an acknowledgment Bundle",
				"operationId": "processMessagePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Acknowledgment Bundle (type: message) with response MessageHeader (code: ok)"},
					"202": map[string]any{"description": "Async message accepted"},
				},
			},
		},
		"/MessageHeader/$process-message": map[string]any{
			"post": map[string]any{
				"tags":        []string{"MessageHeader"},
				"summary":     "HL7 FHIR R4 MessageHeader/$process-message operation",
				"operationId": "messageHeaderProcessMessagePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Acknowledgment Bundle (type: message) with response MessageHeader (code: ok)"},
					"202": map[string]any{"description": "Async message accepted"},
				},
			},
		},
		"/StructureMap/$transform": map[string]any{
			"post": map[string]any{
				"tags":        []string{"StructureMap"},
				"summary":     "HL7 FHIR R4 StructureMap/$transform executing declarative StructureMap group/rule transformations on a source FHIR resource",
				"operationId": "structureMapTransformPost",
				"parameters": []map[string]any{
					{"name": "source", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Transformed target FHIR R4 resource"},
				},
			},
		},
		"/NamingSystem/$preferred-id": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Terminology"},
				"summary":     "HL7 FHIR R4 NamingSystem/$preferred-id translating between uri, oid, uuid, and v2csmnemonic identifier representations",
				"operationId": "namingSystemPreferredIdGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "type", "in": "query", "required": true, "schema": map[string]any{"type": "string", "enum": []string{"oid", "uri", "uuid", "v2csmnemonic"}}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing resolved preferred identifier string"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Terminology"},
				"summary":     "HL7 FHIR R4 NamingSystem/$preferred-id (POST Parameters)",
				"operationId": "namingSystemPreferredIdPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing resolved preferred identifier string"},
				},
			},
		},
		"/Claim/$inquire": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Claim"},
				"summary":     "HL7 Da Vinci PAS Claim/$inquire querying Prior Authorization adjudication status",
				"operationId": "claimInquireGet",
				"parameters": []map[string]any{
					{"name": "claim", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "patient", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "preAuthRef", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: collection) containing adjudicated ClaimResponse and Claim"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Claim"},
				"summary":     "HL7 Da Vinci PAS Claim/$inquire (POST Parameters, Bundle, or Claim)",
				"operationId": "claimInquirePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: collection) containing adjudicated ClaimResponse and Claim"},
				},
			},
		},
		"/Questionnaire/$package": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Questionnaire"},
				"summary":     "HL7 Da Vinci DTR Questionnaire/$package bundling Questionnaire, Library, and ValueSet dependencies",
				"operationId": "questionnairePackageGet",
				"parameters": []map[string]any{
					{"name": "questionnaire", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: collection) containing Questionnaire, Library, and ValueSet resources"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Questionnaire"},
				"summary":     "HL7 Da Vinci DTR Questionnaire/$package (POST Parameters or Questionnaire)",
				"operationId": "questionnairePackagePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: collection) containing Questionnaire, Library, and ValueSet resources"},
				},
			},
		},
		"/CodeSystem/$find-matches": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Terminology"},
				"summary":     "HL7 FHIR R4 CodeSystem/$find-matches property-based concept matching",
				"operationId": "codeSystemFindMatchesGet",
				"parameters": []map[string]any{
					{"name": "system", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "property", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "exact", "in": "query", "required": false, "schema": map[string]any{"type": "boolean"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing matching Coding entries"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Terminology"},
				"summary":     "HL7 FHIR R4 CodeSystem/$find-matches (POST Parameters)",
				"operationId": "codeSystemFindMatchesPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing matching Coding entries"},
				},
			},
		},
		"/$convert": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Discovery & Docs"},
				"summary":     "HL7 FHIR R4 Resource/$convert canonicalizing or converting between FHIR R4 JSON and google.fhir.r4.core ProtoJSON",
				"operationId": "resourceConvertPost",
				"parameters": []map[string]any{
					{"name": "format", "in": "query", "required": false, "schema": map[string]any{"type": "string", "enum": []string{"fhir+json", "proto+json"}}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Converted FHIR R4 resource"},
				},
			},
		},
		"/List/$find": map[string]any{
			"get": map[string]any{
				"tags":        []string{"List"},
				"summary":     "HL7 FHIR R4 List/$find retrieving or synthesizing functional clinical lists (current-medications, current-problems, current-allergies, current-immunizations)",
				"operationId": "listFindGet",
				"parameters": []map[string]any{
					{"name": "patient", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "name", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR R4 List resource"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"List"},
				"summary":     "HL7 FHIR R4 List/$find (POST Parameters)",
				"operationId": "listFindPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR R4 List resource"},
				},
			},
		},
		"/ChargeItem/$apply": map[string]any{
			"post": map[string]any{
				"tags":        []string{"ChargeItem"},
				"summary":     "HL7 FHIR R4 ChargeItem/$apply evaluating ChargeItemDefinition priceComponent rules to compute priceOverride",
				"operationId": "chargeItemApplyPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Priced FHIR R4 ChargeItem resource"},
				},
			},
		},
		"/CoverageEligibilityRequest/$submit": map[string]any{
			"post": map[string]any{
				"tags":        []string{"CoverageEligibilityRequest"},
				"summary":     "HL7 FHIR R4 / Da Vinci Real-Time Eligibility Check evaluating patient Coverage and returning an adjudicated CoverageEligibilityResponse",
				"operationId": "coverageEligibilitySubmitPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Adjudicated FHIR R4 CoverageEligibilityResponse resource"},
				},
			},
		},
		"/ActivityDefinition/$data-requirements": map[string]any{
			"get": map[string]any{
				"tags":        []string{"ActivityDefinition"},
				"summary":     "HL7 FHIR R4 ActivityDefinition/$data-requirements returning a module-definition Library of required clinical resources, ValueSets, and Libraries",
				"operationId": "activityDefinitionDataRequirementsGet",
				"parameters": []map[string]any{
					{"name": "target", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "url", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR R4 module-definition Library resource"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"ActivityDefinition"},
				"summary":     "HL7 FHIR R4 ActivityDefinition/$data-requirements (POST Parameters or ActivityDefinition)",
				"operationId": "activityDefinitionDataRequirementsPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR R4 module-definition Library resource"},
				},
			},
		},
		"/Questionnaire/$data-requirements": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Questionnaire"},
				"summary":     "HL7 FHIR R4 Questionnaire/$data-requirements analyzing Questionnaire items, answerValueSet bindings, and cqf-library extensions into a module-definition Library",
				"operationId": "questionnaireDataRequirementsGet",
				"parameters": []map[string]any{
					{"name": "target", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "url", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR R4 module-definition Library resource"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Questionnaire"},
				"summary":     "HL7 FHIR R4 Questionnaire/$data-requirements (POST Parameters or Questionnaire)",
				"operationId": "questionnaireDataRequirementsPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "FHIR R4 module-definition Library resource"},
				},
			},
		},
		"/GraphDefinition/$graph": map[string]any{
			"get": map[string]any{
				"tags":        []string{"GraphDefinition"},
				"summary":     "HL7 FHIR R4 GraphDefinition/$graph assembling a connected clinical resource Bundle from a start resource reference",
				"operationId": "graphDefinitionGraphGet",
				"parameters": []map[string]any{
					{"name": "graph", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "start", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Collection Bundle containing the start resource and all GraphDefinition-linked resources"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"GraphDefinition"},
				"summary":     "HL7 FHIR R4 GraphDefinition/$graph (POST Parameters with start and graph)",
				"operationId": "graphDefinitionGraphPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Collection Bundle containing the start resource and all GraphDefinition-linked resources"},
				},
			},
		},
		"/$submit-attachment": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Claim", "Task", "DocumentReference"},
				"summary":     "HL7 Da Vinci CDex $submit-attachment persisting clinical attachments for a Claim or PreAuthorization and completing matching CDex Tasks",
				"operationId": "cdexSubmitAttachmentPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "OperationOutcome confirming persisted clinical attachments"},
				},
			},
		},
		"/Task/$submit-attachment": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Task", "DocumentReference"},
				"summary":     "HL7 Da Vinci CDex Task/$submit-attachment operation",
				"operationId": "taskSubmitAttachmentPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "OperationOutcome confirming persisted clinical attachments and Task completion"},
				},
			},
		},
		"/Library/$evaluate": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Library"},
				"summary":     "HL7 FHIR R4 / CPG Library/$evaluate evaluating clinical logic, FHIRPath expressions, and dataRequirements against a subject Patient",
				"operationId": "libraryEvaluateGet",
				"parameters": []map[string]any{
					{"name": "library", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "subject", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "expression", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing evaluated expression values and matching clinical resources"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Library"},
				"summary":     "HL7 FHIR R4 / CPG Library/$evaluate (POST Parameters or Library)",
				"operationId": "libraryEvaluatePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing evaluated expression values and matching clinical resources"},
				},
			},
		},
		"/Encounter/$everything": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Encounter"},
				"summary":     "HL7 FHIR R4 Encounter/$everything assembling all clinical resources in an Encounter compartment",
				"operationId": "encounterEverythingTypeGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "_type", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of Encounter and compartment resources"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Encounter"},
				"summary":     "HL7 FHIR R4 Encounter/$everything (POST Parameters)",
				"operationId": "encounterEverythingTypePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of Encounter and compartment resources"},
				},
			},
		},
		"/Patient/{id}/$health-cards-issue": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Patient", "Immunization"},
				"summary":     "HL7 SMART Health Cards Patient/{id}/$health-cards-issue credential issuance returning signed JWS verifiableCredential and minimized SHC Bundle",
				"operationId": "patientHealthCardsIssueInstancePost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing verifiableCredential (JWS) and resource (SHC Bundle)"},
				},
			},
		},
		"/Patient/$health-cards-issue": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Patient", "Immunization"},
				"summary":     "HL7 SMART Health Cards Patient/$health-cards-issue credential issuance",
				"operationId": "patientHealthCardsIssueTypePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing verifiableCredential (JWS) and resource (SHC Bundle)"},
				},
			},
		},
		"/Patient/{id}/$pcde": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient", "Coverage", "CarePlan", "Claim"},
				"summary":     "HL7 Da Vinci Payer Coverage Decision Exchange (PCDE) Patient/{id}/$pcde assembling continuity-of-coverage document Bundle",
				"operationId": "patientPCDEInstanceGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Document Bundle anchored by a PCDE Composition (LOINC 11503-0) with active CarePlans, Coverage, and prior authorizations"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Patient", "Coverage", "CarePlan", "Claim"},
				"summary":     "HL7 Da Vinci Payer Coverage Decision Exchange (PCDE) Patient/{id}/$pcde (POST Parameters)",
				"operationId": "patientPCDEInstancePost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Document Bundle anchored by a PCDE Composition (LOINC 11503-0)"},
				},
			},
		},
		"/Patient/$pcde": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient", "Coverage", "CarePlan", "Claim"},
				"summary":     "HL7 Da Vinci Payer Coverage Decision Exchange (PCDE) Patient/$pcde",
				"operationId": "patientPCDETypeGet",
				"parameters": []map[string]any{
					{"name": "patient", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Document Bundle anchored by a PCDE Composition (LOINC 11503-0)"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Patient", "Coverage", "CarePlan", "Claim"},
				"summary":     "HL7 Da Vinci Payer Coverage Decision Exchange (PCDE) Patient/$pcde (POST Parameters)",
				"operationId": "patientPCDETypePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Document Bundle anchored by a PCDE Composition (LOINC 11503-0)"},
				},
			},
		},
		"/ResearchStudy/{id}/$evaluate-eligibility": map[string]any{
			"get": map[string]any{
				"tags":        []string{"ResearchStudy", "ResearchSubject", "Group"},
				"summary":     "Evaluate clinical trial eligibility criteria on ResearchStudy/{id} and its enrollment Group for a target Patient",
				"operationId": "researchStudyEvaluateEligibilityInstanceGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "patient", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing eligible boolean, reason, and candidate ResearchSubject"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"ResearchStudy", "ResearchSubject", "Group"},
				"summary":     "Evaluate clinical trial eligibility criteria on ResearchStudy/{id} (POST Parameters)",
				"operationId": "researchStudyEvaluateEligibilityInstancePost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing eligible boolean, reason, and candidate ResearchSubject"},
				},
			},
		},
		"/ResearchStudy/$evaluate-eligibility": map[string]any{
			"get": map[string]any{
				"tags":        []string{"ResearchStudy", "ResearchSubject", "Group"},
				"summary":     "Evaluate clinical trial eligibility criteria on ResearchStudy/$evaluate-eligibility",
				"operationId": "researchStudyEvaluateEligibilityTypeGet",
				"parameters": []map[string]any{
					{"name": "study", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "patient", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing eligible boolean, reason, and candidate ResearchSubject"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"ResearchStudy", "ResearchSubject", "Group"},
				"summary":     "Evaluate clinical trial eligibility criteria on ResearchStudy/$evaluate-eligibility (POST Parameters)",
				"operationId": "researchStudyEvaluateEligibilityTypePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing eligible boolean, reason, and candidate ResearchSubject"},
				},
			},
		},
		"/Claim/$predetermine": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Claim", "ClaimResponse"},
				"summary":     "HL7 FHIR R4 Claim/$predetermine evaluating pre-determination benefit and copay estimations and returning an adjudicated ClaimResponse",
				"operationId": "claimPredetermineTypePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Adjudicated ClaimResponse (use: predetermination)"},
				},
			},
		},
		"/Claim/{id}/$predetermine": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Claim", "ClaimResponse"},
				"summary":     "HL7 FHIR R4 Claim/{id}/$predetermine evaluating pre-determination benefit and copay estimations",
				"operationId": "claimPredetermineInstancePost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Adjudicated ClaimResponse (use: predetermination)"},
				},
			},
		},
		"/Measure/$evaluate-risk-condition-category": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Measure", "MeasureReport"},
				"summary":     "HL7 Da Vinci Risk Adjustment Measure/$evaluate-risk-condition-category evaluating HCC suspect/confirmed coding gaps",
				"operationId": "measureEvaluateRiskConditionCategoryTypeGet",
				"parameters": []map[string]any{
					{"name": "subject", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "periodStart", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "periodEnd", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Collection Bundle containing Risk Adjustment MeasureReport and clinical evidence"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Measure", "MeasureReport"},
				"summary":     "HL7 Da Vinci Risk Adjustment Measure/$evaluate-risk-condition-category (POST Parameters)",
				"operationId": "measureEvaluateRiskConditionCategoryTypePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Collection Bundle containing Risk Adjustment MeasureReport and clinical evidence"},
				},
			},
		},
		"/Measure/{id}/$evaluate-risk-condition-category": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Measure", "MeasureReport"},
				"summary":     "HL7 Da Vinci Risk Adjustment Measure/{id}/$evaluate-risk-condition-category evaluating HCC suspect/confirmed coding gaps",
				"operationId": "measureEvaluateRiskConditionCategoryInstanceGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "subject", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Collection Bundle containing Risk Adjustment MeasureReport and clinical evidence"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Measure", "MeasureReport"},
				"summary":     "HL7 Da Vinci Risk Adjustment Measure/{id}/$evaluate-risk-condition-category (POST Parameters)",
				"operationId": "measureEvaluateRiskConditionCategoryInstancePost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Collection Bundle containing Risk Adjustment MeasureReport and clinical evidence"},
				},
			},
		},
		"/Schedule/$find": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Schedule", "Slot"},
				"summary":     "Discover matching Schedules and free Slots across actor, specialty, service-type, and start/end windows",
				"operationId": "scheduleFindTypeGet",
				"parameters": []map[string]any{
					{"name": "actor", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "specialty", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "service-type", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "start", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "end", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of matching Schedules and free Slots"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Schedule", "Slot"},
				"summary":     "Discover matching Schedules and free Slots (POST Parameters)",
				"operationId": "scheduleFindTypePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of matching Schedules and free Slots"},
				},
			},
		},
		"/Schedule/{id}/$find": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Schedule", "Slot"},
				"summary":     "Discover free Slots for Schedule/{id} across start/end windows",
				"operationId": "scheduleFindInstanceGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "start", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "end", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of Schedule/{id} and free Slots"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Schedule", "Slot"},
				"summary":     "Discover free Slots for Schedule/{id} (POST Parameters)",
				"operationId": "scheduleFindInstancePost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of Schedule/{id} and free Slots"},
				},
			},
		},
		"/ServiceRequest/$crd": map[string]any{
			"post": map[string]any{
				"tags":        []string{"ServiceRequest", "Coverage"},
				"summary":     "HL7 Da Vinci Coverage Requirements Discovery (CRD) evaluating prior-authorization and documentation requirements for a ServiceRequest",
				"operationId": "serviceRequestCRDTypePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters with priorAuthRequired, documentationRequired, questionnaireCanonical, and ext-coverage-information"},
				},
			},
		},
		"/ServiceRequest/{id}/$crd": map[string]any{
			"get": map[string]any{
				"tags":        []string{"ServiceRequest", "Coverage"},
				"summary":     "HL7 Da Vinci Coverage Requirements Discovery (CRD) for ServiceRequest/{id}",
				"operationId": "serviceRequestCRDInstanceGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters with priorAuthRequired, documentationRequired, questionnaireCanonical, and ext-coverage-information"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"ServiceRequest", "Coverage"},
				"summary":     "HL7 Da Vinci Coverage Requirements Discovery (CRD) for ServiceRequest/{id} (POST Parameters)",
				"operationId": "serviceRequestCRDInstancePost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters with priorAuthRequired, documentationRequired, questionnaireCanonical, and ext-coverage-information"},
				},
			},
		},
		"/MedicationRequest/$crd": map[string]any{
			"post": map[string]any{
				"tags":        []string{"MedicationRequest", "Coverage"},
				"summary":     "HL7 Da Vinci Coverage Requirements Discovery (CRD) evaluating prior-authorization and documentation requirements for a MedicationRequest",
				"operationId": "medicationRequestCRDTypePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters with priorAuthRequired, documentationRequired, questionnaireCanonical, and ext-coverage-information"},
				},
			},
		},
		"/MedicationRequest/{id}/$crd": map[string]any{
			"get": map[string]any{
				"tags":        []string{"MedicationRequest", "Coverage"},
				"summary":     "HL7 Da Vinci Coverage Requirements Discovery (CRD) for MedicationRequest/{id}",
				"operationId": "medicationRequestCRDInstanceGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters with priorAuthRequired, documentationRequired, questionnaireCanonical, and ext-coverage-information"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"MedicationRequest", "Coverage"},
				"summary":     "HL7 Da Vinci Coverage Requirements Discovery (CRD) for MedicationRequest/{id} (POST Parameters)",
				"operationId": "medicationRequestCRDInstancePost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters with priorAuthRequired, documentationRequired, questionnaireCanonical, and ext-coverage-information"},
				},
			},
		},
		"/AdverseEvent/$evaluate": map[string]any{
			"get": map[string]any{
				"tags":        []string{"AdverseEvent", "DetectedIssue"},
				"summary":     "Evaluate pharmacovigilance WHO-UMC / Naranjo causality for an AdverseEvent",
				"operationId": "adverseEventEvaluateTypeGet",
				"parameters": []map[string]any{
					{"name": "subject", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters with causalityAssessment, suspectEntityCount, and DetectedIssue"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"AdverseEvent", "DetectedIssue"},
				"summary":     "Evaluate pharmacovigilance WHO-UMC / Naranjo causality for an AdverseEvent (POST)",
				"operationId": "adverseEventEvaluateTypePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters with causalityAssessment, suspectEntityCount, and DetectedIssue"},
				},
			},
		},
		"/AdverseEvent/{id}/$evaluate": map[string]any{
			"get": map[string]any{
				"tags":        []string{"AdverseEvent", "DetectedIssue"},
				"summary":     "Evaluate pharmacovigilance WHO-UMC / Naranjo causality for AdverseEvent/{id}",
				"operationId": "adverseEventEvaluateInstanceGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters with causalityAssessment, suspectEntityCount, and DetectedIssue"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"AdverseEvent", "DetectedIssue"},
				"summary":     "Evaluate pharmacovigilance WHO-UMC / Naranjo causality for AdverseEvent/{id} (POST)",
				"operationId": "adverseEventEvaluateInstancePost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters with causalityAssessment, suspectEntityCount, and DetectedIssue"},
				},
			},
		},
		"/CarePlan/$evaluate": map[string]any{
			"get": map[string]any{
				"tags":        []string{"CarePlan", "Goal"},
				"summary":     "Evaluate CarePlan Goal targets and activity adherence against patient Observations",
				"operationId": "carePlanEvaluateTypeGet",
				"parameters": []map[string]any{
					{"name": "subject", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters with goalsMet, goalsTotal, achievementStatus, and collection Bundle"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"CarePlan", "Goal"},
				"summary":     "Evaluate CarePlan Goal targets and activity adherence (POST)",
				"operationId": "carePlanEvaluateTypePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters with goalsMet, goalsTotal, achievementStatus, and collection Bundle"},
				},
			},
		},
		"/CarePlan/{id}/$evaluate": map[string]any{
			"get": map[string]any{
				"tags":        []string{"CarePlan", "Goal"},
				"summary":     "Evaluate CarePlan/{id} Goal targets and activity adherence against patient Observations",
				"operationId": "carePlanEvaluateInstanceGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters with goalsMet, goalsTotal, achievementStatus, and collection Bundle"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"CarePlan", "Goal"},
				"summary":     "Evaluate CarePlan/{id} Goal targets and activity adherence (POST)",
				"operationId": "carePlanEvaluateInstancePost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters with goalsMet, goalsTotal, achievementStatus, and collection Bundle"},
				},
			},
		},
		"/Patient/{id}/$immds-forecast": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient", "Immunization", "ImmunizationRecommendation"},
				"summary":     "HL7 Immunization Decision Support Forecast (ImmDS) evaluating vaccine history and ACIP/CDC forecast recommendations for Patient/{id}",
				"operationId": "patientImmDSForecastInstanceGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "assessmentDate", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing ImmunizationEvaluation and ImmunizationRecommendation resources"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Patient", "Immunization", "ImmunizationRecommendation"},
				"summary":     "HL7 Immunization Decision Support Forecast (ImmDS) for Patient/{id} (POST Parameters)",
				"operationId": "patientImmDSForecastInstancePost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing ImmunizationEvaluation and ImmunizationRecommendation resources"},
				},
			},
		},
		"/Patient/$immds-forecast": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient", "Immunization", "ImmunizationRecommendation"},
				"summary":     "HL7 Immunization Decision Support Forecast (ImmDS)",
				"operationId": "patientImmDSForecastTypeGet",
				"parameters": []map[string]any{
					{"name": "patient", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing ImmunizationEvaluation and ImmunizationRecommendation resources"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Patient", "Immunization", "ImmunizationRecommendation"},
				"summary":     "HL7 Immunization Decision Support Forecast (ImmDS) (POST Parameters)",
				"operationId": "patientImmDSForecastTypePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing ImmunizationEvaluation and ImmunizationRecommendation resources"},
				},
			},
		},
		"/Claim/$gfe-submit": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Claim", "ExplanationOfBenefit"},
				"summary":     "HL7 Da Vinci Patient Cost Transparency (PCT) Good Faith Estimate submission returning an Advanced Explanation of Benefits (AEOB) Bundle",
				"operationId": "claimGFESubmitTypePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Collection Bundle containing ExplanationOfBenefit (AEOB) and GFE Claim"},
				},
			},
		},
		"/Patient/{id}/$gfe": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient", "Claim", "ExplanationOfBenefit"},
				"summary":     "HL7 Da Vinci Patient Cost Transparency (PCT) Good Faith Estimate & AEOB for Patient/{id}",
				"operationId": "patientGFEInstanceGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Collection Bundle containing ExplanationOfBenefit (AEOB) and GFE Claim"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Patient", "Claim", "ExplanationOfBenefit"},
				"summary":     "HL7 Da Vinci Patient Cost Transparency (PCT) Good Faith Estimate & AEOB for Patient/{id} (POST)",
				"operationId": "patientGFEInstancePost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Collection Bundle containing ExplanationOfBenefit (AEOB) and GFE Claim"},
				},
			},
		},
		"/RiskAssessment/$evaluate": map[string]any{
			"get": map[string]any{
				"tags":        []string{"RiskAssessment"},
				"summary":     "Evaluate clinical ASCVD & LACE 30-day readmission RiskAssessment for a subject Patient",
				"operationId": "riskAssessmentEvaluateTypeGet",
				"parameters": []map[string]any{
					{"name": "subject", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "RiskAssessment resource with prediction probabilityDecimal and qualitativeRisk"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"RiskAssessment"},
				"summary":     "Evaluate clinical ASCVD & LACE 30-day readmission RiskAssessment (POST)",
				"operationId": "riskAssessmentEvaluateTypePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "RiskAssessment resource with prediction probabilityDecimal and qualitativeRisk"},
				},
			},
		},
		"/RiskAssessment/{id}/$evaluate": map[string]any{
			"get": map[string]any{
				"tags":        []string{"RiskAssessment"},
				"summary":     "Evaluate clinical ASCVD & LACE 30-day readmission RiskAssessment/{id}",
				"operationId": "riskAssessmentEvaluateInstanceGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "RiskAssessment resource with prediction probabilityDecimal and qualitativeRisk"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"RiskAssessment"},
				"summary":     "Evaluate clinical ASCVD & LACE 30-day readmission RiskAssessment/{id} (POST)",
				"operationId": "riskAssessmentEvaluateInstancePost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "RiskAssessment resource with prediction probabilityDecimal and qualitativeRisk"},
				},
			},
		},
		"/Composition/$document": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Composition"},
				"summary":     "Generate a FHIR Document Bundle (type: document) from a Composition by id (HL7 FHIR R4 Composition/$document)",
				"operationId": "compositionDocumentTypeGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
					{"name": "persist", "in": "query", "required": false, "schema": map[string]any{"type": "boolean"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Document Bundle with Composition as entry[0] and referenced clinical resources"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Composition"},
				"summary":     "Generate a FHIR Document Bundle (type: document) from an inline Composition or Parameters (POST)",
				"operationId": "compositionDocumentTypePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Document Bundle with Composition as entry[0] and referenced clinical resources"},
				},
			},
		},
		"/Group/{id}/$member-add": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Group"},
				"summary":     "Add an attributed member (Patient) with attributionPeriod to a Da Vinci Member Attribution (ATR) Group roster",
				"operationId": "groupATRMemberAddPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Updated Da Vinci ATR Group resource"},
				},
			},
		},
		"/Group/{id}/$member-remove": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Group"},
				"summary":     "Mark an attributed member (Patient) inactive on a Da Vinci Member Attribution (ATR) Group roster",
				"operationId": "groupATRMemberRemovePost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Updated Da Vinci ATR Group resource"},
				},
			},
		},
		"/Group/{id}/$confirm-attribution-list": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Group"},
				"summary":     "Confirm a Da Vinci Member Attribution (ATR) Group roster and return active/inactive member summary Parameters",
				"operationId": "groupATRConfirmAttributionListGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing status=confirmed, activeMemberCount, inactiveMemberCount, and Group resource"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Group"},
				"summary":     "Confirm a Da Vinci Member Attribution (ATR) Group roster (POST)",
				"operationId": "groupATRConfirmAttributionListPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing status=confirmed, activeMemberCount, inactiveMemberCount, and Group resource"},
				},
			},
		},
		"/MedicationKnowledge/$lookup": map[string]any{
			"get": map[string]any{
				"tags":        []string{"MedicationKnowledge"},
				"summary":     "Look up Da Vinci PDex US Drug Formulary tier & copay details by RxNorm code",
				"operationId": "medicationKnowledgeLookupTypeGet",
				"parameters": []map[string]any{
					{"name": "code", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "MedicationKnowledge resource conforming to usdf-FormularyDrug profile"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"MedicationKnowledge"},
				"summary":     "Look up Da Vinci PDex US Drug Formulary tier & copay details (POST)",
				"operationId": "medicationKnowledgeLookupTypePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "MedicationKnowledge resource conforming to usdf-FormularyDrug profile"},
				},
			},
		},
		"/MedicationKnowledge/{id}/$lookup": map[string]any{
			"get": map[string]any{
				"tags":        []string{"MedicationKnowledge"},
				"summary":     "Look up Da Vinci PDex US Drug Formulary MedicationKnowledge/{id}",
				"operationId": "medicationKnowledgeLookupInstanceGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "MedicationKnowledge resource conforming to usdf-FormularyDrug profile"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"MedicationKnowledge"},
				"summary":     "Look up Da Vinci PDex US Drug Formulary MedicationKnowledge/{id} (POST)",
				"operationId": "medicationKnowledgeLookupInstancePost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "MedicationKnowledge resource conforming to usdf-FormularyDrug profile"},
				},
			},
		},
		"/MedicationRequest/$check-interactions": map[string]any{
			"get": map[string]any{
				"tags":        []string{"MedicationRequest"},
				"summary":     "Evaluate candidate RxNorm medication against patient AllergyIntolerance records and emit DetectedIssue (DALG) alerts",
				"operationId": "medicationRequestCheckInteractionsTypeGet",
				"parameters": []map[string]any{
					{"name": "patient", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "code", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing outcome, interactionCount, and DetectedIssue resources"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"MedicationRequest"},
				"summary":     "Evaluate candidate MedicationRequest against patient AllergyIntolerance records (POST)",
				"operationId": "medicationRequestCheckInteractionsTypePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing outcome, interactionCount, and DetectedIssue resources"},
				},
			},
		},
		"/MedicationRequest/{id}/$check-interactions": map[string]any{
			"get": map[string]any{
				"tags":        []string{"MedicationRequest"},
				"summary":     "Evaluate stored MedicationRequest/{id} against patient AllergyIntolerance records",
				"operationId": "medicationRequestCheckInteractionsInstanceGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing outcome, interactionCount, and DetectedIssue resources"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"MedicationRequest"},
				"summary":     "Evaluate stored MedicationRequest/{id} against patient AllergyIntolerance records (POST)",
				"operationId": "medicationRequestCheckInteractionsInstancePost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing outcome, interactionCount, and DetectedIssue resources"},
				},
			},
		},
		"/$process-notification": map[string]any{
			"post": map[string]any{
				"tags":        []string{"MessageHeader", "Bundle"},
				"summary":     "Process an HL7 Da Vinci Unsolicited Notification Bundle (admit, discharge, transfer, order)",
				"operationId": "processNotificationSystemPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Acknowledgment Bundle (type: message) with response MessageHeader (code: ok)"},
				},
			},
		},
		"/MessageHeader/$process-notification": map[string]any{
			"post": map[string]any{
				"tags":        []string{"MessageHeader"},
				"summary":     "Process an HL7 Da Vinci Unsolicited Notification Bundle on MessageHeader (POST)",
				"operationId": "processNotificationTypePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Acknowledgment Bundle (type: message) with response MessageHeader (code: ok)"},
				},
			},
		},
		"/Patient/$ihe-pix": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient"},
				"summary":     "IHE PIXm (ITI-83) Patient Identifier Cross-Reference Lookup by sourceIdentifier and optional targetSystem",
				"operationId": "patientIHEPixTypeGet",
				"parameters": []map[string]any{
					{"name": "sourceIdentifier", "in": "query", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "targetSystem", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing targetId (Patient reference) and targetIdentifier entries"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Patient"},
				"summary":     "IHE PIXm (ITI-83) Patient Identifier Cross-Reference Lookup (POST)",
				"operationId": "patientIHEPixTypePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing targetId (Patient reference) and targetIdentifier entries"},
				},
			},
		},
		"/Patient/{id}/$ihe-pix": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient"},
				"summary":     "IHE PIXm (ITI-83) Patient Identifier Cross-Reference Lookup for Patient/{id}",
				"operationId": "patientIHEPixInstanceGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "targetSystem", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing targetId (Patient reference) and targetIdentifier entries"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Patient"},
				"summary":     "IHE PIXm (ITI-83) Patient Identifier Cross-Reference Lookup for Patient/{id} (POST)",
				"operationId": "patientIHEPixInstancePost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing targetId (Patient reference) and targetIdentifier entries"},
				},
			},
		},
		"/ServiceRequest/{id}/$collect-specimen": map[string]any{
			"post": map[string]any{
				"tags":        []string{"ServiceRequest", "Specimen"},
				"summary":     "Accession and persist a US Core Specimen linked to laboratory order ServiceRequest/{id}",
				"operationId": "serviceRequestCollectSpecimenPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Persisted US Core Specimen resource with accessionIdentifier"},
				},
			},
		},
		"/DiagnosticReport/$evaluate": map[string]any{
			"get": map[string]any{
				"tags":        []string{"DiagnosticReport"},
				"summary":     "Evaluate a DiagnosticReport and its linked Observation results for abnormal and critical panic values",
				"operationId": "diagnosticReportEvaluateTypeGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing status, abnormalCount, criticalCount, and DetectedIssue alerts"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"DiagnosticReport"},
				"summary":     "Evaluate a DiagnosticReport and its linked Observation results (POST)",
				"operationId": "diagnosticReportEvaluateTypePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing status, abnormalCount, criticalCount, and DetectedIssue alerts"},
				},
			},
		},
		"/DiagnosticReport/{id}/$evaluate": map[string]any{
			"get": map[string]any{
				"tags":        []string{"DiagnosticReport"},
				"summary":     "Evaluate stored DiagnosticReport/{id} and its linked Observation results for abnormal and critical panic values",
				"operationId": "diagnosticReportEvaluateInstanceGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing status, abnormalCount, criticalCount, and DetectedIssue alerts"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"DiagnosticReport"},
				"summary":     "Evaluate stored DiagnosticReport/{id} and its linked Observation results (POST)",
				"operationId": "diagnosticReportEvaluateInstancePost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing status, abnormalCount, criticalCount, and DetectedIssue alerts"},
				},
			},
		},
		"/DiagnosticReport/{id}/$elr-submit": map[string]any{
			"post": map[string]any{
				"tags":        []string{"DiagnosticReport", "MessageHeader", "Bundle"},
				"summary":     "Submit a reportable laboratory DiagnosticReport as an HL7 Public Health Electronic Laboratory Reporting (ELR v1.0.0) message Bundle",
				"operationId": "diagnosticReportELRSubmitPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing reportable, elr-MessageHeader, and elr-message-bundle"},
				},
			},
		},
		"/Observation/$reportable-labs": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Observation"},
				"summary":     "Query reportable laboratory Observations (HL7 Public Health ELR v1.0.0 / RCTC LOINC)",
				"operationId": "observationReportableLabsGet",
				"parameters": []map[string]any{
					{"name": "patient", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of reportable US Core Laboratory Result Observations"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Observation"},
				"summary":     "Query reportable laboratory Observations (POST Parameters)",
				"operationId": "observationReportableLabsPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of reportable US Core Laboratory Result Observations"},
				},
			},
		},
		"/Patient/{id}/$adi-record": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Patient", "Consent", "Observation", "Composition"},
				"summary":     "Record a patient-authored Advance Directive (HL7 PACIO ADI v1.0.0 Consent, Care Experience Preference Observation, and Composition)",
				"operationId": "patientADIRecordPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing PADI-Consent, Care Experience Preference Observation (LOINC 75773-2), and PADI-PACComposition (LOINC 42348-3)"},
				},
			},
		},
		"/Composition/$adi-document": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Composition", "Bundle"},
				"summary":     "Assemble an HL7 PACIO Advance Directive Interoperability (ADI v1.0.0) document Bundle (LOINC 42348-3)",
				"operationId": "compositionADIDocumentGet",
				"parameters": []map[string]any{
					{"name": "patient", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Document Bundle (PADI-AdvanceDirectiveBundle)"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Composition", "Bundle"},
				"summary":     "Assemble an HL7 PACIO Advance Directive Interoperability (ADI v1.0.0) document Bundle (POST)",
				"operationId": "compositionADIDocumentPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Document Bundle (PADI-AdvanceDirectiveBundle)"},
				},
			},
		},
		"/Patient/{id}/$ra-coding-gaps": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient", "MeasureReport", "DetectedIssue"},
				"summary":     "Evaluate CMS-HCC V28 suspect and historic coding gaps for Patient/{id} (HL7 Da Vinci Risk Adjustment v2.0.0)",
				"operationId": "patientRACodingGapsGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Collection Bundle (ra-bundle) containing ra-measurereport and DetectedIssue"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Patient", "MeasureReport", "DetectedIssue"},
				"summary":     "Evaluate CMS-HCC V28 suspect and historic coding gaps for Patient/{id} (POST)",
				"operationId": "patientRACodingGapsPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Collection Bundle (ra-bundle) containing ra-measurereport and DetectedIssue"},
				},
			},
		},
		"/MeasureReport/{id}/$ra-remediate": map[string]any{
			"post": map[string]any{
				"tags":        []string{"MeasureReport", "Condition"},
				"summary":     "Remediate (confirm or refute) a Da Vinci Risk Adjustment HCC coding gap on MeasureReport/{id}",
				"operationId": "measureReportRARemediatePost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing remediationStatus, confirmedCondition, and updated MeasureReport"},
				},
			},
		},
		"/Composition/$eicr-document": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Composition", "Bundle"},
				"summary":     "Assemble an HL7 Public Health Electronic Initial Case Report (eICR v2.1.1, LOINC 55751-2) document Bundle",
				"operationId": "compositionEICRDocumentGet",
				"parameters": []map[string]any{
					{"name": "patient", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Document Bundle (eicr-document-bundle)"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Composition", "Bundle"},
				"summary":     "Assemble an HL7 Public Health Electronic Initial Case Report (eICR v2.1.1) document Bundle (POST)",
				"operationId": "compositionEICRDocumentPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Document Bundle (eicr-document-bundle)"},
				},
			},
		},
		"/Communication/$rr-process": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Communication", "DetectedIssue"},
				"summary":     "Process an HL7 Public Health Reportability Response (RR, LOINC 88085-6) and persist rr-communication & DetectedIssue",
				"operationId": "communicationRRProcessPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing reportabilityStatus, rr-communication, and DetectedIssue"},
				},
			},
		},
		"/Patient/{id}/$pdex-WriteData": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Patient", "Provenance"},
				"summary":     "Ingest payer-to-payer clinical/administrative data for Patient/{id} with US Core / Da Vinci PDex Provenance lineage",
				"operationId": "patientPDexWriteDataPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing persistedCount, pdex-provenance, and OperationOutcome"},
				},
			},
		},
		"/MedicationKnowledge/$formulary-alternatives": map[string]any{
			"get": map[string]any{
				"tags":        []string{"MedicationKnowledge"},
				"summary":     "Evaluate preferred lower-tier therapeutic formulary alternatives (HL7 Da Vinci US Drug Formulary v2.0.1)",
				"operationId": "medicationKnowledgeFormularyAlternativesGet",
				"parameters": []map[string]any{
					{"name": "code", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of preferred tier-1 usdf-FormularyDrug MedicationKnowledge resources"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"MedicationKnowledge"},
				"summary":     "Evaluate preferred lower-tier therapeutic formulary alternatives (POST)",
				"operationId": "medicationKnowledgeFormularyAlternativesPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of preferred tier-1 usdf-FormularyDrug MedicationKnowledge resources"},
				},
			},
		},
		"/Patient/{id}/$record-occupation": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Patient", "Observation"},
				"summary":     "Record structured NIOSH/CDC Occupational Data for Health (ODH v1.3.0) PastOrPresentJob, EmploymentStatus, and CombatZonePeriod Observations",
				"operationId": "patientRecordOccupationPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing jobObservation (LOINC 11341-5), employmentStatus (LOINC 74165-2), and militaryService (LOINC 87511-2)"},
				},
			},
		},
		"/Patient/{id}/$odh-summary": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient", "Observation"},
				"summary":     "Assemble an HL7 Occupational Data for Health (ODH v1.3.0) and Military Service History summary Bundle for Patient/{id}",
				"operationId": "patientODHSummaryGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of ODH Observations (LOINC 11341-5, 74165-2, 87511-2)"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Patient", "Observation"},
				"summary":     "Assemble an HL7 Occupational Data for Health (ODH v1.3.0) and Military Service History summary Bundle (POST)",
				"operationId": "patientODHSummaryPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of ODH Observations (LOINC 11341-5, 74165-2, 87511-2)"},
				},
			},
		},
		"/Patient/{id}/$cancer-registry-report": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient", "Composition", "Condition", "Bundle"},
				"summary":     "Assemble an HL7 Central Cancer Registry Reporting (CCRR v1.0.0, LOINC 72134-0) document Bundle for Patient/{id}",
				"operationId": "patientCancerRegistryReportGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Document Bundle (ccrr-content-bundle)"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Patient", "Composition", "Condition", "Bundle"},
				"summary":     "Assemble an HL7 Central Cancer Registry Reporting (CCRR v1.0.0) document Bundle (POST)",
				"operationId": "patientCancerRegistryReportPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Document Bundle (ccrr-content-bundle)"},
				},
			},
		},
		"/PlanDefinition/$medmorph-kar": map[string]any{
			"get": map[string]any{
				"tags":        []string{"PlanDefinition", "ValueSet", "Bundle"},
				"summary":     "Retrieve an HL7 MedMorph Knowledge Artifact Repository (KAR v1.0.0) specification Bundle",
				"operationId": "planDefinitionMedMorphKARGet",
				"responses": map[string]any{
					"200": map[string]any{"description": "Collection Bundle (us-ph-specification-bundle) containing us-ph-plandefinition and trigger ValueSets"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"PlanDefinition", "ValueSet", "Bundle"},
				"summary":     "Retrieve an HL7 MedMorph Knowledge Artifact Repository (KAR v1.0.0) specification Bundle (POST)",
				"operationId": "planDefinitionMedMorphKARPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Collection Bundle (us-ph-specification-bundle)"},
				},
			},
		},
		"/MeasureReport/$vbpr-performance": map[string]any{
			"get": map[string]any{
				"tags":        []string{"MeasureReport"},
				"summary":     "Generate a Da Vinci Value-Based Performance Report (VBPR v1.0.0 vbp-measurereport)",
				"operationId": "measureReportVBPRPerformanceGet",
				"parameters": []map[string]any{
					{"name": "organization", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Persisted Da Vinci VBPR MeasureReport (vbp-measurereport)"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"MeasureReport"},
				"summary":     "Generate a Da Vinci Value-Based Performance Report (POST)",
				"operationId": "measureReportVBPRPerformancePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Persisted Da Vinci VBPR MeasureReport (vbp-measurereport)"},
				},
			},
		},
		"/MeasureReport/{id}/$vbpr-attest": map[string]any{
			"post": map[string]any{
				"tags":        []string{"MeasureReport", "Provenance"},
				"summary":     "Record a provider/payer contract attestation on Da Vinci VBPR MeasureReport/{id}",
				"operationId": "measureReportVBPRAttestPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing attestationStatus, vbp-measurereport, and Provenance"},
				},
			},
		},
		"/Patient/{id}/$pacio-cognitive-assess": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Patient", "QuestionnaireResponse", "Observation"},
				"summary":     "Record a standardized CMS BIMS Post-Acute Cognitive Status assessment (HL7 PACIO CS v1.0.0)",
				"operationId": "patientPACIOCognitiveAssessPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing cognitiveStatus, pacio-cs-QuestionnaireResponse, and pacio-cs-Observation (LOINC 52491-8)"},
				},
			},
		},
		"/Patient/{id}/$pacio-cognitive-summary": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient", "Observation", "QuestionnaireResponse"},
				"summary":     "Assemble a longitudinal HL7 PACIO Cognitive Status (PACIO CS v1.0.0) summary Bundle for Patient/{id}",
				"operationId": "patientPACIOCognitiveSummaryGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of PACIO Cognitive Status Observations and QuestionnaireResponses"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Patient", "Observation", "QuestionnaireResponse"},
				"summary":     "Assemble a longitudinal HL7 PACIO Cognitive Status summary Bundle (POST)",
				"operationId": "patientPACIOCognitiveSummaryPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of PACIO Cognitive Status Observations and QuestionnaireResponses"},
				},
			},
		},
		"/Claim/$rtpbc-submit": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Claim", "ClaimResponse"},
				"summary":     "Submit a Real-Time Pharmacy Benefit Check (CARIN RTPBC v2.0.0) and receive patient-pay adjudication and formulary alternatives",
				"operationId": "claimRTPBCSubmitPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing patientPayAmount, formularyAlternative, and rtpbc-response-claimresponse"},
				},
			},
		},
		"/ClaimResponse/$rtpbc-coverage": map[string]any{
			"get": map[string]any{
				"tags":        []string{"ClaimResponse", "Bundle"},
				"summary":     "Query CARIN Real-Time Pharmacy Benefit Check (CARIN RTPBC v2.0.0) ClaimResponse determinations",
				"operationId": "claimResponseRTPBCCoverageGet",
				"parameters": []map[string]any{
					{"name": "patient", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of rtpbc-response-claimresponse resources"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"ClaimResponse", "Bundle"},
				"summary":     "Query CARIN Real-Time Pharmacy Benefit Check ClaimResponse determinations (POST)",
				"operationId": "claimResponseRTPBCCoveragePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of rtpbc-response-claimresponse resources"},
				},
			},
		},
		"/Patient/$idi-match": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Patient", "Bundle"},
				"summary":     "Execute NIST SP 800-63-3 IAL2/AAL2 weighted demographic patient matching (HL7 FAST Identity v2.0.0)",
				"operationId": "patientIDIMatchPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of IDI-Patient candidates with search.score"},
				},
			},
		},
		"/Person/{id}/$verify-identity": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Person", "VerificationResult"},
				"summary":     "Verify digital identity credentials for Person/{id} against NIST SP 800-63-3 IAL2 proofing criteria (HL7 FAST Identity v2.0.0)",
				"operationId": "personVerifyIdentityPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing assuranceLevel, Person, and VerificationResult"},
				},
			},
		},
		"/Encounter/{id}/$pacio-timepoint": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Encounter", "ClinicalImpression"},
				"summary":     "Record a Post-Acute Care Re-Assessment Timepoint Encounter and ClinicalImpression (HL7 PACIO RT v1.0.0)",
				"operationId": "encounterPACIOTimepointPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing timepointEncounter and clinicalImpression"},
				},
			},
		},
		"/Encounter/{id}/$pacio-timepoints": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Encounter", "ClinicalImpression", "Bundle"},
				"summary":     "Assemble a searchset Bundle of Post-Acute Re-Assessment Timepoint Encounters and ClinicalImpressions (HL7 PACIO RT v1.0.0)",
				"operationId": "encounterPACIOTimepointsGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of PACIO Re-Assessment Timepoints"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Encounter", "ClinicalImpression", "Bundle"},
				"summary":     "Assemble a searchset Bundle of Post-Acute Re-Assessment Timepoint Encounters and ClinicalImpressions (POST)",
				"operationId": "encounterPACIOTimepointsPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of PACIO Re-Assessment Timepoints"},
				},
			},
		},
		"/Composition/$vrdr-document": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Composition", "Patient", "Condition", "Observation", "Bundle"},
				"summary":     "Assemble an NCHS Vital Records Death Reporting (VRDR v2.2.0, LOINC 64297-5) death certificate document Bundle",
				"operationId": "compositionVRDRDocumentGet",
				"parameters": []map[string]any{
					{"name": "patient", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Document Bundle (vrdr-death-certificate-document)"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Composition", "Patient", "Condition", "Observation", "Bundle"},
				"summary":     "Assemble an NCHS Vital Records Death Reporting (VRDR v2.2.0) death certificate document Bundle (POST)",
				"operationId": "compositionVRDRDocumentPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Document Bundle (vrdr-death-certificate-document)"},
				},
			},
		},
		"/Bundle/$vrdr-submit": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Bundle", "MessageHeader", "Condition"},
				"summary":     "Submit a Vital Records Death Record to NCHS/STEVE and receive TRX/MRE coded cause-of-death response (VRDR v2.2.0)",
				"operationId": "bundleVRDRSubmitPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Message Bundle (vrdr-cause-of-death-coded-bundle)"},
				},
			},
		},
		"/Task/$cdex-request": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Task", "Provenance"},
				"summary":     "Create and fulfill a Da Vinci CDex v2.0.0 Task-based clinical data request (cdex-task-data-request)",
				"operationId": "taskCDexRequestPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Persisted Da Vinci CDex Task (cdex-task-data-request)"},
				},
			},
		},
		"/Task/{id}/$cdex-poll": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Task", "Provenance", "Bundle"},
				"summary":     "Poll a Da Vinci CDex v2.0.0 Task-based data request and retrieve HRex Provenance and fulfilled clinical data",
				"operationId": "taskCDexPollGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle containing cdex-task-data-request and hrex-provenance"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Task", "Provenance", "Bundle"},
				"summary":     "Poll a Da Vinci CDex v2.0.0 Task-based data request (POST)",
				"operationId": "taskCDexPollPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle containing cdex-task-data-request and hrex-provenance"},
				},
			},
		},
		"/Patient/{id}/$pacio-pfe-record": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Patient", "Observation", "Goal"},
				"summary":     "Record an ICF-coded Personal Functioning and Engagement observation and functional Goal (HL7 PACIO PFE v1.0.0)",
				"operationId": "patientPACIOPFERecordPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing pfeObservation and pfeGoal"},
				},
			},
		},
		"/Patient/{id}/$pacio-pfe-summary": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient", "Observation", "Goal", "Bundle"},
				"summary":     "Assemble a longitudinal HL7 PACIO Personal Functioning and Engagement (PACIO PFE v1.0.0) summary Bundle for Patient/{id}",
				"operationId": "patientPACIOPFESummaryGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of PACIO PFE Observations and Goals"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Patient", "Observation", "Goal", "Bundle"},
				"summary":     "Assemble a longitudinal HL7 PACIO Personal Functioning and Engagement summary Bundle (POST)",
				"operationId": "patientPACIOPFESummaryPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of PACIO PFE Observations and Goals"},
				},
			},
		},
		"/Composition/$bfdr-document": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Composition", "Patient", "Observation", "Bundle"},
				"summary":     "Assemble an NCHS Vital Records Birth and Fetal Death Reporting (BFDR v1.1.0, LOINC 68998-4) live birth certificate document Bundle",
				"operationId": "compositionBFDRDocumentGet",
				"parameters": []map[string]any{
					{"name": "patient", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Document Bundle (Bundle-document-live-birth-report)"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Composition", "Patient", "Observation", "Bundle"},
				"summary":     "Assemble an NCHS Vital Records Birth and Fetal Death Reporting (BFDR v1.1.0) live birth certificate document Bundle (POST)",
				"operationId": "compositionBFDRDocumentPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Document Bundle (Bundle-document-live-birth-report)"},
				},
			},
		},
		"/Bundle/$bfdr-submit": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Bundle", "MessageHeader", "Observation"},
				"summary":     "Submit a Vital Records Live Birth Record to NCHS/STEVE and receive BFDR acknowledgment message Bundle (BFDR v1.1.0)",
				"operationId": "bundleBFDRSubmitPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Message Bundle (Bundle-ack-live-birth)"},
				},
			},
		},
		"/Communication/$cdex-submit-attachment": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Communication", "Parameters"},
				"summary":     "Submit a Da Vinci CDex v2.0.0 clinical attachment Communication (cdex-attachment-communication)",
				"operationId": "communicationCDexSubmitAttachmentPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing status, trackingId, and persisted Communication"},
				},
			},
		},
		"/Task/$cdex-questionnaire-request": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Task", "QuestionnaireResponse"},
				"summary":     "Create and fulfill a Da Vinci CDex v2.0.0 Questionnaire Task data request (cdex-task-data-request / cdex-sdc-questionnaireresponse)",
				"operationId": "taskCDexQuestionnaireRequestPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Completed Task with QuestionnaireResponse output"},
				},
			},
		},
		"/Procedure/$mcode-radiotherapy-record": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Procedure", "Condition"},
				"summary":     "Record an HL7 CodeX / mCODE v3.0.0 Radiotherapy Course Summary Procedure (mcode-radiotherapy-course-summary)",
				"operationId": "procedureMCODERadiotherapyRecordPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Persisted mcode-radiotherapy-course-summary Procedure"},
				},
			},
		},
		"/Patient/{id}/$mcode-radiotherapy-summary": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient", "Procedure", "Condition", "Bundle"},
				"summary":     "Assemble an HL7 CodeX / mCODE v3.0.0 Radiotherapy Course Summary Bundle for Patient/{id}",
				"operationId": "patientMCODERadiotherapySummaryGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of mCODE Radiotherapy Procedures and Primary Cancer Conditions"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Patient", "Procedure", "Condition", "Bundle"},
				"summary":     "Assemble an HL7 CodeX / mCODE v3.0.0 Radiotherapy Course Summary Bundle (POST)",
				"operationId": "patientMCODERadiotherapySummaryPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of mCODE Radiotherapy Procedures and Primary Cancer Conditions"},
				},
			},
		},
		"/Immunization/$iis-submit": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Immunization", "ImmunizationEvaluation", "ImmunizationRecommendation"},
				"summary":     "Submit an administered Immunization to an Immunization Information System (IIS / HL7 ImmDS v2.0.0) and evaluate dose validity",
				"operationId": "immunizationIISSubmitPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing accepted status, Immunization, ImmunizationEvaluation, and ImmunizationRecommendation"},
				},
			},
		},
		"/Patient/{id}/$iis-history": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient", "Immunization", "ImmunizationEvaluation", "ImmunizationRecommendation", "Bundle"},
				"summary":     "Query longitudinal Immunization Information System (IIS / HL7 ImmDS v2.0.0) history and evaluated forecast for Patient/{id}",
				"operationId": "patientIISHistoryGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of Immunizations, ImmunizationEvaluations, and ImmunizationRecommendations"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Patient", "Immunization", "ImmunizationEvaluation", "ImmunizationRecommendation", "Bundle"},
				"summary":     "Query longitudinal Immunization Information System (IIS) history and forecast (POST)",
				"operationId": "patientIISHistoryPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of Immunizations, ImmunizationEvaluations, and ImmunizationRecommendations"},
				},
			},
		},
		"/Bundle/$cdex-sign": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Bundle", "Provenance"},
				"summary":     "Digitally sign a clinical data Bundle per HL7 Da Vinci CDex v2.0.0 Digital Signatures (cdex-signature-bundle)",
				"operationId": "bundleCDexSignPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Signed cdex-signature-bundle with JWS Bundle.signature and HRex Provenance"},
				},
			},
		},
		"/Bundle/$cdex-verify-signature": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Bundle", "Parameters", "OperationOutcome"},
				"summary":     "Verify a cryptographic JWS digital signature on a Da Vinci CDex v2.0.0 signed Bundle",
				"operationId": "bundleCDexVerifySignaturePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing verified boolean, signatureType, signer, and OperationOutcome"},
				},
			},
		},
		"/DiagnosticReport/$mcode-genomics-report": map[string]any{
			"post": map[string]any{
				"tags":        []string{"DiagnosticReport", "Observation"},
				"summary":     "Record an HL7 CodeX / mCODE v3.0.0 Cancer Genomics Report (mcode-cancer-genomics-report, LOINC 81247-9) with genomic variant and tumor marker",
				"operationId": "diagnosticReportMCODEGenomicsReportPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Persisted mcode-cancer-genomics-report DiagnosticReport"},
				},
			},
		},
		"/Patient/{id}/$mcode-genomics-summary": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient", "DiagnosticReport", "Observation", "Bundle"},
				"summary":     "Assemble an HL7 CodeX / mCODE v3.0.0 Cancer Genomics and Tumor Marker Summary Bundle for Patient/{id}",
				"operationId": "patientMCODEGenomicsSummaryGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of mCODE Cancer Genomics Reports, Genomic Variants, and Tumor Markers"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Patient", "DiagnosticReport", "Observation", "Bundle"},
				"summary":     "Assemble an HL7 CodeX / mCODE v3.0.0 Cancer Genomics and Tumor Marker Summary Bundle (POST)",
				"operationId": "patientMCODEGenomicsSummaryPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of mCODE Cancer Genomics Reports, Genomic Variants, and Tumor Markers"},
				},
			},
		},
		"/DiagnosticReport/$nbs-submit": map[string]any{
			"post": map[string]any{
				"tags":        []string{"DiagnosticReport", "Specimen", "Observation"},
				"summary":     "Submit an HL7 Public Health Newborn Screening (NBS / EHDI v1.0.0, LOINC 54089-8) panel with dried blood spot Specimen and screening Observation",
				"operationId": "diagnosticReportNBSSubmitPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing accepted status, DiagnosticReport, Specimen, and Observation"},
				},
			},
		},
		"/Patient/{id}/$nbs-summary": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient", "DiagnosticReport", "Specimen", "Observation", "Bundle"},
				"summary":     "Assemble an HL7 Public Health Newborn Screening (NBS / EHDI v1.0.0) summary Bundle for Patient/{id}",
				"operationId": "patientNBSSummaryGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of Newborn Screening DiagnosticReports, Specimens, and Observations"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Patient", "DiagnosticReport", "Specimen", "Observation", "Bundle"},
				"summary":     "Assemble an HL7 Public Health Newborn Screening summary Bundle (POST)",
				"operationId": "patientNBSSummaryPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of Newborn Screening DiagnosticReports, Specimens, and Observations"},
				},
			},
		},
		"/ExplanationOfBenefit/$pct-aeob-summary": map[string]any{
			"get": map[string]any{
				"tags":        []string{"ExplanationOfBenefit", "Bundle"},
				"summary":     "Assemble a Da Vinci Patient Cost Transparency (PCT v2.0.0) Advanced Explanation of Benefits packet Bundle (davinci-pct-aeob-packet)",
				"operationId": "eobPCTAEOBSummaryGet",
				"parameters": []map[string]any{
					{"name": "patient", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle (davinci-pct-aeob-packet) containing davinci-pct-aeob"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"ExplanationOfBenefit", "Bundle"},
				"summary":     "Assemble a Da Vinci Patient Cost Transparency Advanced Explanation of Benefits packet Bundle (POST)",
				"operationId": "eobPCTAEOBSummaryPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle (davinci-pct-aeob-packet) containing davinci-pct-aeob"},
				},
			},
		},
		"/Task/$pct-gfe-coordination": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Task"},
				"summary":     "Create and fulfill a Da Vinci Patient Cost Transparency (PCT v2.0.0) multi-provider Good Faith Estimate coordination Task (davinci-pct-gfe-coordination-task)",
				"operationId": "taskPCTGFEoordinationPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Completed davinci-pct-gfe-coordination-task"},
				},
			},
		},
		"/AdverseEvent/$vulcan-icsr-report": map[string]any{
			"post": map[string]any{
				"tags":        []string{"AdverseEvent", "Composition", "Bundle"},
				"summary":     "Generate an FDA / ICH E2B(R3) Individual Case Safety Report (ICSR, LOINC 85893-6) document Bundle from an HL7 Vulcan clinical research AdverseEvent",
				"operationId": "adverseEventVulcanICSRReportPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Document Bundle (icsr-bundle) containing icsr-composition and vulcan-adverse-event-clinical-research"},
				},
			},
		},
		"/ResearchSubject/{id}/$vulcan-soa-timeline": map[string]any{
			"get": map[string]any{
				"tags":        []string{"ResearchSubject", "CarePlan", "Encounter", "Bundle"},
				"summary":     "Assemble an HL7 Vulcan Schedule of Activities (SOA v1.0.0) study visit timeline Bundle for ResearchSubject/{id}",
				"operationId": "researchSubjectVulcanSOATimelineGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle containing vulcan-soa-study-visit-plan CarePlan and vulcan-soa-encounter"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"ResearchSubject", "CarePlan", "Encounter", "Bundle"},
				"summary":     "Assemble an HL7 Vulcan Schedule of Activities study visit timeline Bundle (POST)",
				"operationId": "researchSubjectVulcanSOATimelinePost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle containing vulcan-soa-study-visit-plan CarePlan and vulcan-soa-encounter"},
				},
			},
		},
		"/Bundle/$ecr-submit": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Bundle", "Composition", "Communication", "Parameters"},
				"summary":     "Submit an HL7 Public Health Electronic Initial Case Report (eICR v2.1.2, LOINC 55751-2) Bundle and receive a Reportability Response (RR, LOINC 88085-6)",
				"operationId": "bundleECRSubmitPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing status, eicrBundle, reportabilityResponse Composition, and rr-communication"},
				},
			},
		},
		"/Patient/{id}/$ecr-summary": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient", "Composition", "Communication", "Bundle"},
				"summary":     "Assemble an HL7 Public Health Electronic Case Reporting (eCR v2.1.2) summary Bundle for Patient/{id}",
				"operationId": "patientECRSummaryGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle containing eICR Composition, Reportability Response Composition, and Communication"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Patient", "Composition", "Communication", "Bundle"},
				"summary":     "Assemble an HL7 Public Health eCR summary Bundle (POST)",
				"operationId": "patientECRSummaryPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle containing eICR Composition, Reportability Response Composition, and Communication"},
				},
			},
		},
		"/QuestionnaireResponse/$sdoh-assess": map[string]any{
			"post": map[string]any{
				"tags":        []string{"QuestionnaireResponse", "Observation", "Condition", "Goal", "Parameters"},
				"summary":     "Evaluate an HL7 Gravity Project SDOH v2.2.0 screening QuestionnaireResponse and extract SDOHCC Observation, Condition, and Goal",
				"operationId": "questionnaireResponseSDOHAssessPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing status, SDOHCC-ObservationScreeningResponse, SDOHCC-Condition, and SDOHCC-Goal"},
				},
			},
		},
		"/Task/$sdoh-fulfill": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Task", "Procedure", "Parameters"},
				"summary":     "Fulfill an HL7 Gravity Project SDOH v2.2.0 CBO referral Task (SDOHCC-TaskForReferralManagement) and persist delivered SDOHCC-Procedure",
				"operationId": "taskSDOHFulfillPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing status, completed Task, and SDOHCC-Procedure"},
				},
			},
		},
		"/ResearchSubject/$vulcan-esource-extract": map[string]any{
			"post": map[string]any{
				"tags":        []string{"ResearchSubject", "QuestionnaireResponse", "Provenance", "Parameters"},
				"summary":     "Extract EHR clinical data for a ResearchSubject into an HL7 Vulcan eSource Case Report Form (vulcan-esource-crf-response) with Provenance lineage",
				"operationId": "researchSubjectVulcanESourceExtractPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing status, ResearchSubject, vulcan-esource-crf-response QuestionnaireResponse, and Provenance"},
				},
			},
		},
		"/ResearchStudy/{id}/$vulcan-summary": map[string]any{
			"get": map[string]any{
				"tags":        []string{"ResearchStudy", "ResearchSubject", "QuestionnaireResponse", "CarePlan", "AdverseEvent", "Bundle"},
				"summary":     "Assemble an HL7 Vulcan Real-World Data (RWD) & eSource study summary Bundle for ResearchStudy/{id}",
				"operationId": "researchStudyVulcanSummaryGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle containing ResearchStudy, enrolled ResearchSubjects, eSource CRFs, SOA CarePlans, and AdverseEvents"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"ResearchStudy", "ResearchSubject", "QuestionnaireResponse", "CarePlan", "AdverseEvent", "Bundle"},
				"summary":     "Assemble an HL7 Vulcan RWD & eSource study summary Bundle (POST)",
				"operationId": "researchStudyVulcanSummaryPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle containing ResearchStudy, enrolled ResearchSubjects, eSource CRFs, SOA CarePlans, and AdverseEvents"},
				},
			},
		},
		"/Patient/{id}/$elr-summary": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient", "DiagnosticReport", "Observation", "Specimen", "Bundle"},
				"summary":     "Assemble an HL7 Public Health Electronic Laboratory Reporting (ELR v1.0.0) summary Bundle for Patient/{id}",
				"operationId": "patientELRSummaryGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle (us-ph-reportable-lab-bundle) containing reportable DiagnosticReport, Observation, and Specimen"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Patient", "DiagnosticReport", "Observation", "Specimen", "Bundle"},
				"summary":     "Assemble an HL7 Public Health ELR summary Bundle (POST)",
				"operationId": "patientELRSummaryPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle (us-ph-reportable-lab-bundle) containing reportable DiagnosticReport, Observation, and Specimen"},
				},
			},
		},
		"/Bundle/$cancer-registry-submit": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Bundle", "Composition", "Condition", "Communication", "Parameters"},
				"summary":     "Submit an HL7 Central Cancer Registry Reporting (CCRR v1.0.0, LOINC 72134-0) Bundle and persist ccrr-composition and mcode-primary-cancer-condition",
				"operationId": "bundleCancerRegistrySubmitPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing status, ccrr-content-bundle, ccrr-composition, mcode-primary-cancer-condition, and Communication"},
				},
			},
		},
		"/ExplanationOfBenefit/$carin-bb-claims": map[string]any{
			"get": map[string]any{
				"tags":        []string{"ExplanationOfBenefit", "Bundle"},
				"summary":     "Assemble an HL7 CARIN Blue Button v2.0.0 Oral & Vision ExplanationOfBenefit claims Bundle",
				"operationId": "explanationOfBenefitCARINBBClaimsGet",
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle containing C4BB-ExplanationOfBenefit-Oral and C4BB-ExplanationOfBenefit-Vision"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"ExplanationOfBenefit", "Bundle"},
				"summary":     "Assemble an HL7 CARIN Blue Button v2.0.0 Oral & Vision ExplanationOfBenefit claims Bundle (POST)",
				"operationId": "explanationOfBenefitCARINBBClaimsPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle containing C4BB-ExplanationOfBenefit-Oral and C4BB-ExplanationOfBenefit-Vision"},
				},
			},
		},
		"/Patient/{id}/$rtpbc-summary": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient", "Claim", "ClaimResponse", "Bundle"},
				"summary":     "Assemble an HL7 CARIN Real-Time Pharmacy Benefit Check (RTPBC v1.0.0) summary Bundle for Patient/{id}",
				"operationId": "patientRTPBCSummaryGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle containing rtpbc-claim-response and rtpbc-claim"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Patient", "Claim", "ClaimResponse", "Bundle"},
				"summary":     "Assemble an HL7 CARIN RTPBC summary Bundle (POST)",
				"operationId": "patientRTPBCSummaryPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle containing rtpbc-claim-response and rtpbc-claim"},
				},
			},
		},
		"/Composition/$pacio-toc-bundle": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Composition", "Bundle"},
				"summary":     "Generate and persist an HL7 PACIO Post-Acute Care Transitions of Care (TOC v1.0.0, LOINC 18842-5) discharge summary document Bundle",
				"operationId": "compositionPACIOTOCBundlePost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Document Bundle (pacio-toc-bundle) containing pacio-toc-composition"},
				},
			},
		},
		"/Consent/$adi-verify": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Consent", "Provenance", "Parameters"},
				"summary":     "Verify and attest an HL7 PACIO Advance Directives Interoperability (ADI v1.0.0, LOINC 42348-3) Consent with Provenance signature",
				"operationId": "consentADIVerifyPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing status, verified, PADI-ParticipantConsent, and Provenance"},
				},
			},
		},
		"/MeasureReport/$nhsn-aur-submit": map[string]any{
			"post": map[string]any{
				"tags":        []string{"MeasureReport", "MedicationAdministration", "Parameters"},
				"summary":     "Submit a CDC NHSN Antimicrobial Use and Resistance (AUR / ACH v1.0.0) digital quality MeasureReport and persist antimicrobial MedicationAdministration",
				"operationId": "measureReportNHSNAURSubmitPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing status, nhsn-aur-measure-report, and ach-daily-medicationadministration"},
				},
			},
		},
		"/Patient/{id}/$nhsn-hai-summary": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient", "MeasureReport", "Encounter", "MedicationAdministration", "Bundle"},
				"summary":     "Assemble a CDC NHSN HAI & AUR acute care surveillance Bundle for Patient/{id}",
				"operationId": "patientNHSNHAISummaryGet",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle containing nhsn-aur-measure-report, ach-daily-encounter, and ach-daily-medicationadministration"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"Patient", "MeasureReport", "Encounter", "MedicationAdministration", "Bundle"},
				"summary":     "Assemble a CDC NHSN HAI & AUR acute care surveillance Bundle (POST)",
				"operationId": "patientNHSNHAISummaryPost",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle containing nhsn-aur-measure-report, ach-daily-encounter, and ach-daily-medicationadministration"},
				},
			},
		},
		"/Patient/$mcode-trial-match": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Patient", "ResearchStudy", "Bundle"},
				"summary":     "Match an HL7 mCODE oncology Patient against clinical trials (CodeX Clinical Trial Matching v1.0.0)",
				"operationId": "patientMCODETrialMatchPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle (codex-clinical-trial-match-bundle) of matched ResearchStudy resources"},
				},
			},
		},
		"/Observation/$mcode-disease-status": map[string]any{
			"post": map[string]any{
				"tags":        []string{"Observation", "Parameters"},
				"summary":     "Record and persist an HL7 mCODE v3.0.0 Cancer Disease Status Observation (LOINC 97509-4)",
				"operationId": "observationMCODEDiseaseStatusPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing status and mcode-cancer-disease-status Observation"},
				},
			},
		},
		"/OrganizationAffiliation/$fast-network-query": map[string]any{
			"get": map[string]any{
				"tags":        []string{"OrganizationAffiliation", "Organization", "Endpoint", "Bundle"},
				"summary":     "Query ONC FAST National Directory (NDH v1.0.0) & Da Vinci Plan-Net v1.1.0 OrganizationAffiliation and payer network directory",
				"operationId": "organizationAffiliationFASTNetworkQueryGet",
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of ndh-OrganizationAffiliation, plannet-Network Organization, and ndh-Endpoint"},
				},
			},
			"post": map[string]any{
				"tags":        []string{"OrganizationAffiliation", "Organization", "Endpoint", "Bundle"},
				"summary":     "Query ONC FAST NDH & Da Vinci Plan-Net OrganizationAffiliation and payer network directory (POST)",
				"operationId": "organizationAffiliationFASTNetworkQueryPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Searchset Bundle of ndh-OrganizationAffiliation, plannet-Network Organization, and ndh-Endpoint"},
				},
			},
		},
		"/HealthcareService/$fast-attest": map[string]any{
			"post": map[string]any{
				"tags":        []string{"HealthcareService", "VerificationResult", "Parameters"},
				"summary":     "Attest and verify an ONC FAST National Directory (NDH v1.0.0) HealthcareService and persist VerificationResult",
				"operationId": "healthcareServiceFASTAttestPost",
				"responses": map[string]any{
					"200": map[string]any{"description": "Parameters containing status, ndh-HealthcareService, and ndh-Verification VerificationResult"},
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
			{"name": "_filter", "in": "query", "description": "FHIR R4 boolean search filter expression (and, or, not, parentheses, eq, ne, co, sw, ew, gt, lt, ge, le, sa, eb, ap, pr, po, ss, sb, in, ni, re)", "schema": map[string]any{"type": "string"}},
			{"name": "_content", "in": "query", "description": "Full-text search across indexed string and token display values", "schema": map[string]any{"type": "string"}},
			{"name": "_text", "in": "query", "description": "Narrative text.div search", "schema": map[string]any{"type": "string"}},
		}
		seenParam := map[string]bool{"_id": true, "_lastUpdated": true, "_profile": true, "_tag": true, "_security": true, "_source": true, "_filter": true, "_content": true, "_text": true}
		for _, sp := range mergeCustomSearchParams(rt, spDefs[rt]) {
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

		paths["/"+rt+"/$reindex"] = map[string]any{
			"post": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Rebuild spidx_* covering search indexes for all active %s resources ($reindex)", rt),
				"operationId": "typeReindex" + rt,
				"responses": map[string]any{
					"200": map[string]any{"description": "OperationOutcome summarizing reindexed resources"},
				},
			},
		}

		paths["/"+rt+"/{id}/$reindex"] = map[string]any{
			"post": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Rebuild spidx_* covering search indexes for %s/{id} ($reindex)", rt),
				"operationId": "instanceReindex" + rt,
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "OperationOutcome summarizing reindexed resource"},
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
	s.syncTenantCustomSearchParams(r.Context(), tenant)

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
