package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

const serviceBaseURLTimestamp = "2026-01-15T00:00:00Z"

func (s *Server) resolvedTenantAndOrigin(r *http.Request) (string, string) {
	tenant := strings.TrimSpace(r.PathValue("tenant"))
	if tenant == "" {
		tenant = "demo"
	}
	return tenant, requestOrigin(r)
}

func writePublicFHIRCollectionBundle(w http.ResponseWriter, r *http.Request, bundle map[string]any) {
	raw, err := json.Marshal(bundle)
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", err.Error())
		return
	}
	sum := sha256.Sum256(raw)
	etag := `W/"` + hex.EncodeToString(sum[:8]) + `"`

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
	if r == nil || r.Method != http.MethodHead {
		_, _ = w.Write(raw)
	}
}

// handleServiceBaseURLs serves the ONC HTI-1 § 170.404(b)(2) Service Base URL Publication Bundle
// at GET /.well-known/service-base-urls and GET /fhir/r4/{tenant}/.well-known/service-base-urls.
func (s *Server) handleServiceBaseURLs(w http.ResponseWriter, r *http.Request) {
	tenant, baseOrigin := s.resolvedTenantAndOrigin(r)
	fhirBase := baseOrigin + "/fhir/r4/" + tenant
	epFullURL := fhirBase + "/Endpoint/flint-r4-endpoint-primary"
	parentOrgFullURL := fhirBase + "/Organization/org-flint-health-system"
	childOrgFullURL := fhirBase + "/Organization/org-flint-regional-clinic"

	bundle := map[string]any{
		"resourceType": "Bundle",
		"id":           "flint-service-base-urls-" + tenant,
		"type":         "collection",
		"timestamp":    serviceBaseURLTimestamp,
		"entry": []map[string]any{
			{
				"fullUrl": epFullURL,
				"resource": map[string]any{
					"resourceType": "Endpoint",
					"id":           "flint-r4-endpoint-primary",
					"extension": []map[string]any{
						{
							"url":       "http://hl7.org/fhir/StructureDefinition/endpoint-fhir-version",
							"valueCode": "4.0.1",
						},
					},
					"status": "active",
					"connectionType": map[string]any{
						"system": "http://terminology.hl7.org/CodeSystem/endpoint-connection-type",
						"code":   "hl7-fhir-rest",
					},
					"name": "Flint FHIR R4 Endpoint (" + tenant + ")",
					"payloadType": []map[string]any{
						{
							"coding": []map[string]any{
								{
									"system": "http://terminology.hl7.org/CodeSystem/endpoint-payload-type",
									"code":   "none",
								},
							},
						},
					},
					"address": fhirBase,
				},
			},
			{
				"fullUrl": parentOrgFullURL,
				"resource": map[string]any{
					"resourceType": "Organization",
					"id":           "org-flint-health-system",
					"identifier": []map[string]any{
						{
							"system": "http://hl7.org/fhir/sid/us-npi",
							"value":  "1407071236",
						},
					},
					"active": true,
					"name":   "Flint Health System",
					"address": []map[string]any{
						{
							"line":       []string{"100 Clinical Way"},
							"city":       "Boston",
							"state":      "MA",
							"postalCode": "02110",
							"country":    "US",
						},
					},
					"endpoint": []map[string]any{
						{
							"reference": "Endpoint/flint-r4-endpoint-primary",
							"display":   "Flint FHIR R4 Endpoint (" + tenant + ")",
						},
					},
				},
			},
			{
				"fullUrl": childOrgFullURL,
				"resource": map[string]any{
					"resourceType": "Organization",
					"id":           "org-flint-regional-clinic",
					"identifier": []map[string]any{
						{
							"system": "http://hl7.org/fhir/sid/us-npi",
							"value":  "1376083733",
						},
					},
					"active": true,
					"name":   "Flint Regional Outpatient Clinic",
					"address": []map[string]any{
						{
							"line":       []string{"250 Harbor Blvd"},
							"city":       "Cambridge",
							"state":      "MA",
							"postalCode": "02140",
							"country":    "US",
						},
					},
					"partOf": map[string]any{
						"reference": "Organization/org-flint-health-system",
						"display":   "Flint Health System",
					},
				},
			},
		},
	}

	writePublicFHIRCollectionBundle(w, r, bundle)
}

// handleSMARTBrands serves the HL7 SMART App Launch STU2.2 User-Access Brands Bundle
// at GET /.well-known/smart-brands.json and GET /fhir/r4/{tenant}/.well-known/smart-brands.json.
func (s *Server) handleSMARTBrands(w http.ResponseWriter, r *http.Request) {
	tenant, baseOrigin := s.resolvedTenantAndOrigin(r)
	fhirBase := baseOrigin + "/fhir/r4/" + tenant
	epFullURL := fhirBase + "/Endpoint/flint-r4-endpoint-primary"
	orgFullURL := fhirBase + "/Organization/org-flint-brand"

	bundle := map[string]any{
		"resourceType": "Bundle",
		"id":           "flint-smart-brands-" + tenant,
		"meta": map[string]any{
			"profile": []string{
				"http://hl7.org/fhir/smart-app-launch/StructureDefinition/user-access-brands-bundle",
			},
		},
		"type":      "collection",
		"timestamp": serviceBaseURLTimestamp,
		"entry": []map[string]any{
			{
				"fullUrl": orgFullURL,
				"resource": map[string]any{
					"resourceType": "Organization",
					"id":           "org-flint-brand",
					"meta": map[string]any{
						"profile": []string{
							"http://hl7.org/fhir/smart-app-launch/StructureDefinition/user-access-brand",
						},
					},
					"extension": []map[string]any{
						{
							"url": "http://hl7.org/fhir/StructureDefinition/organization-brand",
							"extension": []map[string]any{
								{
									"url":      "brandLogo",
									"valueUrl": baseOrigin + "/fhir/r4/" + tenant + "/$docs",
								},
							},
						},
						{
							"url": "http://hl7.org/fhir/StructureDefinition/organization-portal",
							"extension": []map[string]any{
								{
									"url":         "portalName",
									"valueString": "Flint Patient Health Portal",
								},
								{
									"url":      "portalUrl",
									"valueUrl": fhirBase + "/$docs",
								},
								{
									"url":           "portalDescription",
									"valueMarkdown": "Patient portal and SMART on FHIR API access for Flint Health System.",
								},
								{
									"url": "portalEndpoint",
									"valueReference": map[string]any{
										"reference": epFullURL,
										"display":   "Flint FHIR R4 Endpoint",
									},
								},
							},
						},
					},
					"identifier": []map[string]any{
						{
							"system": "urn:ietf:rfc:3986",
							"value":  baseOrigin,
						},
					},
					"active": true,
					"type": []map[string]any{
						{
							"coding": []map[string]any{
								{
									"system":  "http://terminology.hl7.org/CodeSystem/organization-type",
									"code":    "prov",
									"display": "Healthcare Provider",
								},
							},
						},
					},
					"name":  "Flint Health System",
					"alias": []string{"Flint FHIR"},
					"telecom": []map[string]any{
						{
							"system": "url",
							"value":  fhirBase + "/$docs",
						},
					},
					"address": []map[string]any{
						{
							"city":  "Boston",
							"state": "MA",
						},
					},
					"endpoint": []map[string]any{
						{
							"reference": epFullURL,
							"display":   "Flint FHIR R4 Endpoint",
						},
					},
				},
			},
			{
				"fullUrl": epFullURL,
				"resource": map[string]any{
					"resourceType": "Endpoint",
					"id":           "flint-r4-endpoint-primary",
					"meta": map[string]any{
						"profile": []string{
							"http://hl7.org/fhir/smart-app-launch/StructureDefinition/user-access-endpoint",
						},
					},
					"extension": []map[string]any{
						{
							"url":       "http://hl7.org/fhir/StructureDefinition/endpoint-fhir-version",
							"valueCode": "4.0.1",
						},
					},
					"status": "active",
					"connectionType": map[string]any{
						"system": "http://terminology.hl7.org/CodeSystem/endpoint-connection-type",
						"code":   "hl7-fhir-rest",
					},
					"name": "Flint FHIR R4 Endpoint",
					"contact": []map[string]any{
						{
							"system": "url",
							"value":  baseOrigin,
						},
					},
					"payloadType": []map[string]any{
						{
							"coding": []map[string]any{
								{
									"system": "http://terminology.hl7.org/CodeSystem/endpoint-payload-type",
									"code":   "none",
								},
							},
						},
					},
					"address": fhirBase,
				},
			},
		},
	}

	writePublicFHIRCollectionBundle(w, r, bundle)
}
