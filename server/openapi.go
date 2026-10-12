package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/flint-fhir/flint/pkg/fhirutil"
	"github.com/flint-fhir/flint/pkg/validation"
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
			Capability:    "Proto3 Wire Binary Store (Zero JSONB)",
			Specification: "Google FHIR R4 Protobuf + 6 HAPI spidx_* Covering Tables",
			Status:        "Verified",
			Endpoint:      "/fhir/r4/{tenant}/{resourceType}",
		},
		{
			Category:      "FHIR R4 CRUD, Versioning & Patch",
			Capability:    "read, vread, create, update, patch (RFC 6902 & Merge Patch), delete, history-instance, ETag / If-Match CAS",
			Specification: "HL7 FHIR R4 RESTful API (4.0.1) & RFC 6902 / RFC 7396",
			Status:        "Verified",
			Endpoint:      "/fhir/r4/{tenant}/{resourceType}/{id}",
		},
		{
			Category:      "Conditional Operations & Bundles",
			Capability:    "Conditional Create (If-None-Exist), Conditional Update, Conditional Delete & ACID Transaction / Batch Bundles",
			Specification: "HL7 FHIR R4 Bundle & Conditional Operations",
			Status:        "Verified",
			Endpoint:      "/fhir/r4/{tenant}/{resourceType}?identifier=...",
		},
		{
			Category:      "Advanced Search Engine",
			Capability:    "Date/Quantity Ranges, Chained Search, _include / _revinclude",
			Specification: "HL7 FHIR R4 Search",
			Status:        "Verified",
			Endpoint:      "/fhir/r4/{tenant}/{resourceType}?_include=...",
		},
		{
			Category:      "Patient Compartment Operations",
			Capability:    "Patient/$everything Longitudinal Record Assembly",
			Specification: "HL7 FHIR R4 OperationDefinition/Patient-everything",
			Status:        "Verified",
			Endpoint:      "/fhir/r4/{tenant}/Patient/{id}/$everything",
		},
		{
			Category:      "Bulk Data Access",
			Capability:    "System & Patient $export (NDJSON Stream + Async Polling)",
			Specification: "HL7 FHIR Bulk Data Access IG STU2 / ONC (g)(10)",
			Status:        "Verified",
			Endpoint:      "/fhir/r4/{tenant}/$export",
		},
		{
			Category:      "Conformance & Validation",
			Capability:    "StructureDefinition & ValueSet Validation ($validate)",
			Specification: "HL7 FHIR R4 OperationDefinition/Resource-validate",
			Status:        "Verified",
			Endpoint:      "/fhir/r4/{tenant}/{resourceType}/$validate",
		},
		{
			Category:      "Security & Authorization",
			Capability:    "SMART on FHIR v2 Scopes, OIDC/Zitadel JWT & Patient Compartment Isolation",
			Specification: "SMART App Launch 2.0.0",
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
			Capability:    "AutoMQ CDC -> Iceberg Table Topics -> SQLMesh Patient 360 Marts",
			Specification: "Apache Iceberg + SQLMesh (DuckDB)",
			Status:        "Verified",
			Endpoint:      "analytics/models/mart_patient_360.sql",
		},
	}
}

func resourceSearchParamDefs() map[string][]map[string]string {
	return map[string][]map[string]string{
		"Patient": {
			{"name": "family", "type": "string"},
			{"name": "given", "type": "string"},
			{"name": "identifier", "type": "token"},
			{"name": "gender", "type": "token"},
			{"name": "birthdate", "type": "date"},
			{"name": "organization", "type": "reference"},
		},
		"Condition": {
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
			{"name": "class", "type": "token"},
			{"name": "status", "type": "token"},
			{"name": "type", "type": "token"},
			{"name": "patient", "type": "reference"},
			{"name": "subject", "type": "reference"},
			{"name": "date", "type": "date"},
			{"name": "participant", "type": "reference"},
		},
		"Observation": {
			{"name": "code", "type": "token"},
			{"name": "status", "type": "token"},
			{"name": "category", "type": "token"},
			{"name": "patient", "type": "reference"},
			{"name": "subject", "type": "reference"},
			{"name": "date", "type": "date"},
			{"name": "performer", "type": "reference"},
			{"name": "value-quantity", "type": "quantity"},
			{"name": "component-value-quantity", "type": "quantity"},
			{"name": "combo-value-quantity", "type": "quantity"},
		},
		"Practitioner": {
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
			{"name": "code", "type": "token"},
			{"name": "status", "type": "token"},
			{"name": "category", "type": "token"},
			{"name": "patient", "type": "reference"},
			{"name": "subject", "type": "reference"},
			{"name": "encounter", "type": "reference"},
			{"name": "result", "type": "reference"},
			{"name": "date", "type": "date"},
			{"name": "issued", "type": "date"},
		},
		"Immunization": {
			{"name": "vaccine-code", "type": "token"},
			{"name": "status", "type": "token"},
			{"name": "patient", "type": "reference"},
			{"name": "subject", "type": "reference"},
			{"name": "date", "type": "date"},
			{"name": "lot-number", "type": "string"},
		},
		"Organization": {
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
	}
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
	resOrder := []string{
		"Patient", "Condition", "Encounter", "Observation", "Practitioner",
		"MedicationRequest", "AllergyIntolerance", "Procedure", "DiagnosticReport",
		"Immunization", "Organization",
	}
	seenRes := make(map[string]bool, len(resOrder))
	for _, rt := range resOrder {
		seenRes[rt] = true
	}
	var extraRes []string
	for rt := range s.protoRegistry {
		if !seenRes[rt] {
			extraRes = append(extraRes, rt)
		}
	}
	sort.Strings(extraRes)
	resOrder = append(resOrder, extraRes...)

	for _, rt := range resOrder {
		sch := defaultSchemas[rt]
		md := s.protoRegistry[rt]
		if md != nil {
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
		"description": "HL7 FHIR R4 Bundle (searchset, transaction, batch, history)",
		"required":    []string{"resourceType", "type"},
		"properties": map[string]any{
			"resourceType": map[string]any{"type": "string", "enum": []string{"Bundle"}},
			"type":         map[string]any{"type": "string", "enum": []string{"transaction", "batch", "searchset", "history"}},
			"total":        map[string]any{"type": "integer"},
			"entry":        map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
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
		"/$export": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Bulk Data ($export)"},
				"summary":     "System-level FHIR Bulk Data Export (ONC (g)(10) NDJSON)",
				"operationId": "systemBulkExport",
				"parameters": []map[string]any{
					{"name": "_type", "in": "query", "description": "Comma-separated FHIR resource types (e.g. Patient,Observation)", "schema": map[string]any{"type": "string"}},
					{"name": "_since", "in": "query", "description": "Only include resources updated at or after this ISO-8601 timestamp", "schema": map[string]any{"type": "string"}},
					{"name": "_outputFormat", "in": "query", "description": "NDJSON output format (default application/fhir+ndjson)", "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Newline-delimited FHIR JSON (application/fhir+ndjson) or BulkExportManifest"},
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
					{"name": "_outputFormat", "in": "query", "description": "NDJSON output format (application/fhir+ndjson)", "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Newline-delimited FHIR JSON (application/fhir+ndjson)"},
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
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Newline-delimited FHIR JSON (application/fhir+ndjson)"},
					"202": map[string]any{"description": "Async job accepted"},
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
		"/Patient/{id}/$everything": map[string]any{
			"get": map[string]any{
				"tags":        []string{"Patient"},
				"summary":     "Assemble full Patient compartment longitudinal Bundle ($everything)",
				"operationId": "patientEverything",
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "_type", "in": "query", "description": "Comma-separated resource types", "schema": map[string]any{"type": "string"}},
					{"name": "_since", "in": "query", "description": "ISO-8601 timestamp lower bound", "schema": map[string]any{"type": "string"}},
					{"name": "_count", "in": "query", "schema": map[string]any{"type": "integer"}},
					{"name": "_offset", "in": "query", "schema": map[string]any{"type": "integer"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Bundle (type: searchset) containing Patient and compartment resources"},
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
			{"name": "_count", "in": "query", "description": "Page size (default 20, max 100)", "schema": map[string]any{"type": "integer"}},
			{"name": "_offset", "in": "query", "description": "Pagination offset", "schema": map[string]any{"type": "integer"}},
			{"name": "_include", "in": "query", "description": "Forward reference include (e.g. Observation:patient)", "schema": map[string]any{"type": "string"}},
			{"name": "_revinclude", "in": "query", "description": "Reverse reference include (e.g. Observation:patient)", "schema": map[string]any{"type": "string"}},
		}
		for _, sp := range spDefs[rt] {
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
				"summary":     fmt.Sprintf("Create %s resource (supports If-None-Exist conditional create)", rt),
				"operationId": "create" + rt,
				"parameters": []map[string]any{
					{"name": "If-None-Exist", "in": "header", "required": false, "description": "FHIR conditional create search query (e.g. identifier=http://hospital.example.org/mrn|MRN-001)", "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"201": map[string]any{"description": "Created " + rt + " (with ETag and Location headers)"},
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

		paths["/"+rt+"/{id}"] = map[string]any{
			"get": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Read %s by ID", rt),
				"operationId": "read" + rt,
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": rt + " resource with weak ETag header"},
					"404": map[string]any{"description": "Not found"},
					"410": map[string]any{"description": "Soft-deleted tombstone"},
				},
			},
			"put": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Update or Create-on-Update %s (supports If-Match CAS)", rt),
				"operationId": "update" + rt,
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "If-Match", "in": "header", "required": false, "description": `Expected version ETag (e.g. W/"1") for optimistic concurrency control`, "schema": map[string]any{"type": "string"}},
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
				"summary":     fmt.Sprintf("Soft-delete %s and clear search indexes", rt),
				"operationId": "delete" + rt,
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"204": map[string]any{"description": "Deleted (tombstone version recorded in _history)"},
				},
			},
		}

		paths["/"+rt+"/{id}/_history"] = map[string]any{
			"get": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("List version history for %s/{id}", rt),
				"operationId": "history" + rt,
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "_count", "in": "query", "schema": map[string]any{"type": "integer"}},
					{"name": "_offset", "in": "query", "schema": map[string]any{"type": "integer"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "History Bundle"},
				},
			},
		}

		paths["/"+rt+"/{id}/_history/{vid}"] = map[string]any{
			"get": map[string]any{
				"tags":        []string{rt},
				"summary":     fmt.Sprintf("Read specific version (vread) of %s/{id}", rt),
				"operationId": "vread" + rt,
				"parameters": []map[string]any{
					{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "vid", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
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
				"summary":     fmt.Sprintf("Validate %s resource against StructureDefinition & ValueSets", rt),
				"operationId": "validate" + rt,
				"responses": map[string]any{
					"200": map[string]any{"description": "OperationOutcome validation result"},
				},
			},
		}
	}

	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "Flint FHIR R4 Server & Lakehouse API",
			"version":     "0.1.0",
			"description": "Proto-native HL7 FHIR R4 operational store (Zero JSONB in Postgres) with HAPI covering search indexes, SMART on FHIR v2 authorization, HIPAA/ONC AuditEvent logging, Bulk Data ($export), and AutoMQ -> Iceberg -> SQLMesh lakehouse.",
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
					"description":  "SMART on FHIR v1/v2 OAuth2 JWT Bearer Token (e.g. system/*.read, user/*.*, patient/*.read)",
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
  <title>Flint FHIR R4 API &amp; Conformance Explorer ({{TENANT}})</title>
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
    .brand { display: flex; align-items: center; gap: 0.75rem; }
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
    .controls { display: flex; gap: 0.75rem; align-items: center; flex-wrap: wrap; }
    input[type="text"] {
      background: var(--panel);
      border: 1px solid var(--border);
      color: var(--text);
      padding: 0.45rem 0.75rem;
      border-radius: 0.375rem;
      font-size: 0.85rem;
      min-width: 240px;
    }
    .btn {
      background: var(--accent);
      color: #fff;
      border: none;
      padding: 0.45rem 0.9rem;
      border-radius: 0.375rem;
      font-weight: 600;
      font-size: 0.85rem;
      cursor: pointer;
      text-decoration: none;
    }
    main { max-width: 1280px; margin: 0 auto; padding: 1.5rem 2rem; }
    h2 { font-size: 1.15rem; margin-top: 2rem; border-bottom: 1px solid var(--border); padding-bottom: 0.5rem; }
    table { width: 100%; border-collapse: collapse; background: var(--panel); border-radius: 0.5rem; overflow: hidden; font-size: 0.875rem; }
    th, td { text-align: left; padding: 0.65rem 0.9rem; border-bottom: 1px solid var(--border); }
    th { background: rgba(15, 23, 42, 0.6); color: var(--muted); font-weight: 600; }
    .status-ok { color: var(--green); font-weight: 600; }
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
    }
  </style>
</head>
<body>
  <header>
    <div class="brand">
      <h1>🔥 Flint FHIR R4 API &amp; Conformance Explorer</h1>
      <span class="badge">FHIR R4 4.0.1</span>
      <span class="badge">google/fhir Proto3 BYTEA (0 JSONB)</span>
      <span class="badge">ONC (g)(10) Bulk $export</span>
    </div>
    <div class="controls">
      <input type="text" id="bearerToken" placeholder="Optional SMART Bearer JWT..." />
      <a class="btn" href="/fhir/r4/{{TENANT}}/openapi.json" target="_blank">OpenAPI 3.1 JSON</a>
      <a class="btn" href="/fhir/r4/{{TENANT}}/metadata" target="_blank">CapabilityStatement</a>
    </div>
  </header>
  <main>
    <h2>1. FHIR R4 / US Core / ONC (g)(10) Conformance Matrix</h2>
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

    <h2>2. Interactive FHIR R4 Endpoint Explorer</h2>
    <div id="endpointsContainer">Loading OpenAPI 3.1 specification...</div>

    <h2>3. Live Response Inspector</h2>
    <pre id="liveOutput">Click "Try GET" on any GET endpoint above to inspect live FHIR R4 JSON / NDJSON output.</pre>
  </main>
  <script>
    const tenant = "{{TENANT}}";
    const specUrl = "/fhir/r4/" + tenant + "/openapi.json";

    async function runTryIt(path) {
      const out = document.getElementById("liveOutput");
      const token = document.getElementById("bearerToken").value.trim();
      const headers = {};
      if (token) headers["Authorization"] = "Bearer " + token;
      const fullUrl = "/fhir/r4/" + tenant + (path === "/" ? "" : path);
      out.textContent = "Requesting GET " + fullUrl + " ...";
      try {
        const resp = await fetch(fullUrl, { headers });
        const text = await resp.text();
        out.textContent = "HTTP " + resp.status + " (" + (resp.headers.get("Content-Type") || "") + ")\n\n" + text;
      } catch (err) {
        out.textContent = "Error: " + err;
      }
    }

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
              ? ` + "`" + `<button class="btn" onclick="runTryIt('${path}')">Try GET</button>` + "`" + `
              : "";
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
