package fhirxml_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"hegel.dev/go/hegel"

	"github.com/flint-fhir/flint/pkg/fhirutil"
	"github.com/flint-fhir/flint/pkg/fhirxml"
)

func decodeCanonicalNumberJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func marshalUseNumber(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func assertFourWayIsomorphism(ht *hegel.T, resType string, md protoreflect.MessageDescriptor, rawMap map[string]any) {
	json0, err := marshalUseNumber(rawMap)
	if err != nil {
		ht.Fatalf("%s marshal json0: %v", resType, err)
	}

	// Step 1: JSON_0 -> XML_1 -> JSON_1
	xml1, err := fhirxml.MarshalFHIRJSON(json0)
	if err != nil {
		ht.Fatalf("%s MarshalFHIRJSON(json0) failed: %v\njson0=%s", resType, err, string(json0))
	}
	json1, err := fhirxml.UnmarshalFHIRJSON(xml1)
	if err != nil {
		ht.Fatalf("%s UnmarshalFHIRJSON(xml1) failed: %v\nxml1=%s", resType, err, string(xml1))
	}

	parsed0, err := decodeCanonicalNumberJSON(json0)
	if err != nil {
		ht.Fatalf("%s decode json0: %v", resType, err)
	}
	parsed1, err := decodeCanonicalNumberJSON(json1)
	if err != nil {
		ht.Fatalf("%s decode json1: %v", resType, err)
	}
	if !reflect.DeepEqual(parsed0, parsed1) {
		ht.Fatalf("%s JSON->XML->JSON mismatch (including exact Decimal precision):\njson0: %s\nxml1:  %s\njson1: %s",
			resType, string(json0), string(xml1), string(json1))
	}

	// Step 2: XML_1 -> JSON_1 -> XML_2 (canonical XML byte-for-byte idempotence)
	xml2, err := fhirxml.MarshalFHIRJSON(json1)
	if err != nil {
		ht.Fatalf("%s MarshalFHIRJSON(json1) failed: %v", resType, err)
	}
	if !bytes.Equal(xml1, xml2) {
		ht.Fatalf("%s XML canonical idempotence violated:\nxml1: %s\nxml2: %s", resType, string(xml1), string(xml2))
	}

	// Step 3: JSON_1 -> ProtoJSON -> Proto3 Binary Wire -> JSON_2 -> XML_3
	normBytes, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, json1)
	if err != nil {
		ht.Fatalf("%s NormalizeFHIRJSONToProtoJSON failed: %v\njson1=%s", resType, err, string(json1))
	}
	dynMsg := dynamicpb.NewMessage(md)
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(normBytes, dynMsg); err != nil {
		ht.Fatalf("%s strict protojson.Unmarshal failed: %v\nnorm=%s", resType, err, string(normBytes))
	}
	protoWire, err := proto.Marshal(dynMsg)
	if err != nil {
		ht.Fatalf("%s proto.Marshal failed: %v", resType, err)
	}
	json2, err := fhirutil.DenormalizeProtoJSONToFHIRJSON(md, resType, protoWire, 0, time.Time{})
	if err != nil {
		ht.Fatalf("%s DenormalizeProtoJSONToFHIRJSON failed: %v", resType, err)
	}

	parsed2, err := decodeCanonicalNumberJSON(json2)
	if err != nil {
		ht.Fatalf("%s decode json2: %v", resType, err)
	}
	if !reflect.DeepEqual(parsed1, parsed2) {
		ht.Fatalf("%s Proto3 wire round-trip mismatch:\njson1: %s\njson2: %s", resType, string(json1), string(json2))
	}

	xml3, err := fhirxml.MarshalFHIRJSON(json2)
	if err != nil {
		ht.Fatalf("%s MarshalFHIRJSON(json2) failed: %v", resType, err)
	}
	if !bytes.Equal(xml1, xml3) {
		ht.Fatalf("%s 4-way XML_1 != XML_3 after Proto3 binary round-trip:\nxml1: %s\nxml3: %s", resType, string(xml1), string(xml3))
	}
}

// TestProperty_FHIRXMLProto3FourWayIsomorphism verifies lossless 4-way round-trip isomorphism
// (JSON <-> XML <-> Proto3 binary wire <-> JSON <-> XML) across polymorphic Patient, Observation,
// Encounter, and Bundle resources including trailing-zero Decimals, _field primitive extensions,
// choice[x] elements, XHTML Narrative.div, contained resources, and Reference child ordering.
func TestProperty_FHIRXMLProto3FourWayIsomorphism(t *testing.T) {
	descs := fhirutil.AllR4ResourceDescriptors()
	patMD := descs["Patient"]
	obsMD := descs["Observation"]
	encMD := descs["Encounter"]
	bundleMD := fhirutil.BundleDescriptor()

	hegel.Test(t, func(ht *hegel.T) {
		patID := hegel.Draw(ht, hegel.FromRegex(`pat-[a-z0-9]{2,8}`, true))
		obsID := hegel.Draw(ht, hegel.FromRegex(`obs-[a-z0-9]{2,8}`, true))
		orgID := hegel.Draw(ht, hegel.FromRegex(`org-[a-z0-9]{2,6}`, true))
		family := hegel.Draw(ht, hegel.FromRegex(`[A-Z][a-z]{2,10}`, true))
		given := hegel.Draw(ht, hegel.FromRegex(`[A-Z][a-z]{2,10}`, true))
		gender := hegel.Draw(ht, hegel.SampledFrom([]string{"male", "female", "other", "unknown"}))
		active := hegel.Draw(ht, hegel.Booleans())
		birthYear := hegel.Draw(ht, hegel.Integers(1940, 2024))
		birthMonth := hegel.Draw(ht, hegel.Integers(1, 12))
		birthDay := hegel.Draw(ht, hegel.Integers(1, 28))
		birthDate := fmt.Sprintf("%04d-%02d-%02d", birthYear, birthMonth, birthDay)

		includePrimExt := hegel.Draw(ht, hegel.Booleans())
		includeContained := hegel.Draw(ht, hegel.Booleans())
		includeNarrative := hegel.Draw(ht, hegel.Booleans())
		deceasedMode := hegel.Draw(ht, hegel.SampledFrom([]string{"none", "bool", "dateTime"}))
		multiBirthMode := hegel.Draw(ht, hegel.SampledFrom([]string{"none", "bool", "integer"}))

		patMap := map[string]any{
			"resourceType": "Patient",
			"id":           patID,
			"meta": map[string]any{
				"versionId":   "1",
				"lastUpdated": "2026-04-01T10:15:30Z",
			},
			"active":    active,
			"gender":    gender,
			"birthDate": birthDate,
			"name": []any{
				map[string]any{
					"use":    "official",
					"family": family,
					"given":  []any{given},
				},
			},
		}

		if includePrimExt {
			patMap["_gender"] = map[string]any{
				"id": "gen-ext-1",
				"extension": []any{
					map[string]any{
						"url":         "http://example.org/fhir/StructureDefinition/gender-source",
						"valueString": "chart-verified",
					},
				},
			}
			// Sparse array primitive extension with null alignment!
			patMap["name"].([]any)[0].(map[string]any)["given"] = []any{given, nil}
			patMap["name"].([]any)[0].(map[string]any)["_given"] = []any{
				nil,
				map[string]any{
					"extension": []any{
						map[string]any{
							"url":       "http://hl7.org/fhir/StructureDefinition/data-absent-reason",
							"valueCode": "masked",
						},
					},
				},
			}
		}

		if includeNarrative {
			patMap["text"] = map[string]any{
				"status": "generated",
				"div":    fmt.Sprintf(`<div xmlns="http://www.w3.org/1999/xhtml"><p>Patient <b>%s</b> &amp; record</p></div>`, family),
			}
		}

		if includeContained {
			patMap["contained"] = []any{
				map[string]any{
					"resourceType": "Organization",
					"id":           orgID,
					"active":       true,
					"name":         "Clinic " + family,
				},
			}
			patMap["managingOrganization"] = map[string]any{
				"extension": []any{
					map[string]any{
						"url":         "http://example.org/fhir/StructureDefinition/ref-confidence",
						"valueString": "high",
					},
				},
				"reference": "#" + orgID,
				"type":      "Organization",
				"identifier": map[string]any{
					"system": "http://hl7.org/fhir/sid/us-npi",
					"value":  "1234567890",
				},
				"display": "Clinic " + family,
			}
		}

		switch deceasedMode {
		case "bool":
			patMap["deceasedBoolean"] = hegel.Draw(ht, hegel.Booleans())
		case "dateTime":
			patMap["deceasedDateTime"] = "2026-02-14T08:30:00Z"
		}
		switch multiBirthMode {
		case "bool":
			patMap["multipleBirthBoolean"] = false
		case "integer":
			patMap["multipleBirthInteger"] = json.Number(fmt.Sprintf("%d", hegel.Draw(ht, hegel.Integers(1, 4))))
		}

		assertFourWayIsomorphism(ht, "Patient", patMD, patMap)

		// Polymorphic Observation with exact trailing-zero Decimals and choice[x] values.
		decimalLexeme := hegel.Draw(ht, hegel.SampledFrom([]string{
			"98.60", "120.00", "0.250", "37.500", "-4.50", "72.5", "0.00", "100",
		}))
		obsChoice := hegel.Draw(ht, hegel.SampledFrom([]string{"quantity", "string", "boolean", "codeableConcept"}))
		obsStatus := hegel.Draw(ht, hegel.SampledFrom([]string{"registered", "preliminary", "final", "amended"}))

		obsMap := map[string]any{
			"resourceType": "Observation",
			"id":           obsID,
			"status":       obsStatus,
			"code": map[string]any{
				"coding": []any{
					map[string]any{
						"system":  "http://loinc.org",
						"code":    "8310-5",
						"display": "Body temperature",
					},
				},
			},
			"subject": map[string]any{
				"reference": "Patient/" + patID,
				"display":   given + " " + family,
			},
			"effectiveDateTime": "2026-04-01T09:00:00Z",
		}

		switch obsChoice {
		case "quantity":
			obsMap["valueQuantity"] = map[string]any{
				"value":  json.Number(decimalLexeme),
				"unit":   "Cel",
				"system": "http://unitsofmeasure.org",
				"code":   "Cel",
			}
		case "string":
			obsMap["valueString"] = "Specimen note: " + family
		case "boolean":
			obsMap["valueBoolean"] = active
		case "codeableConcept":
			obsMap["valueCodeableConcept"] = map[string]any{
				"coding": []any{
					map[string]any{
						"system": "http://snomed.info/sct",
						"code":   "260385009",
					},
				},
				"text": "Negative",
			}
		}

		assertFourWayIsomorphism(ht, "Observation", obsMD, obsMap)

		// Encounter (testing class <-> class_value XML/Proto3 mapping)
		encClass := hegel.Draw(ht, hegel.SampledFrom([]string{"AMB", "IMP", "EMER"}))
		encMap := map[string]any{
			"resourceType": "Encounter",
			"id":           "enc-" + patID,
			"status":       "finished",
			"class": map[string]any{
				"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode",
				"code":   encClass,
			},
			"subject": map[string]any{
				"reference": "Patient/" + patID,
			},
		}
		assertFourWayIsomorphism(ht, "Encounter", encMD, encMap)

		// Bundle wrapping generated Patient + Observation + Encounter inline resources!
		bundleType := hegel.Draw(ht, hegel.SampledFrom([]string{"collection", "searchset"}))
		bundleMap := map[string]any{
			"resourceType": "Bundle",
			"id":           "bundle-" + patID,
			"type":         bundleType,
			"entry": []any{
				map[string]any{
					"fullUrl":  "urn:uuid:11111111-1111-4111-8111-111111111111",
					"resource": patMap,
				},
				map[string]any{
					"fullUrl":  "urn:uuid:22222222-2222-4222-8222-222222222222",
					"resource": obsMap,
				},
				map[string]any{
					"fullUrl":  "urn:uuid:33333333-3333-4333-8333-333333333333",
					"resource": encMap,
				},
			},
		}
		if bundleType == "searchset" {
			bundleMap["total"] = json.Number("3")
		}
		assertFourWayIsomorphism(ht, "Bundle", bundleMD, bundleMap)
	}, hegel.WithTestCases(80))
}

// TestProperty_FHIRXMLSecurityDTDAndEntityRejection verifies that arbitrary XML payloads
// containing DOCTYPE, ENTITY, external SYSTEM/PUBLIC DTDs, or embedded Narrative DOCTYPEs
// are deterministically rejected without panicking.
func TestProperty_FHIRXMLSecurityDTDAndEntityRejection(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		resType := hegel.Draw(ht, hegel.SampledFrom([]string{"Patient", "Observation", "Bundle", "OperationOutcome"}))
		entityName := hegel.Draw(ht, hegel.FromRegex(`[a-z]{2,8}`, true))
		targetURI := hegel.Draw(ht, hegel.SampledFrom([]string{
			"file:///etc/passwd",
			"http://169.254.169.254/latest/meta-data/",
			"ftp://internal.example/secret",
		}))
		variant := hegel.Draw(ht, hegel.Integers(0, 3))

		var payload string
		switch variant {
		case 0:
			payload = fmt.Sprintf(`<?xml version="1.0"?><!DOCTYPE %s [<!ENTITY %s SYSTEM %q>]><%s xmlns="http://hl7.org/fhir"><id value="&%s;"/></%s>`,
				resType, entityName, targetURI, resType, entityName, resType)
		case 1:
			payload = fmt.Sprintf(`<!DOCTYPE %s PUBLIC "-//W3C//DTD XHTML 1.0//EN" %q><%s xmlns="http://hl7.org/fhir"><id value="1"/></%s>`,
				resType, targetURI, resType, resType)
		case 2:
			payload = fmt.Sprintf(`<!DOCTYPE lolz [<!ENTITY %s "boom"><!ENTITY %s2 "&%s;&%s;">]><%s xmlns="http://hl7.org/fhir"><id value="1"/></%s>`,
				entityName, entityName, entityName, entityName, resType, resType)
		default:
			payload = fmt.Sprintf(`<%s xmlns="http://hl7.org/fhir"><text><status value="generated"/><div xmlns="http://www.w3.org/1999/xhtml"><!DOCTYPE html><p>bad</p></div></text></%s>`,
				resType, resType)
		}

		if _, err := fhirxml.UnmarshalFHIRJSON([]byte(payload)); err == nil {
			ht.Fatalf("expected UnmarshalFHIRJSON to reject DTD/ENTITY payload:\n%s", payload)
		} else if !strings.Contains(strings.ToLower(err.Error()), "forbidden") && !strings.Contains(strings.ToLower(err.Error()), "invalid") && !strings.Contains(strings.ToLower(err.Error()), "xml") {
			ht.Fatalf("unexpected error message for DTD/ENTITY rejection: %v", err)
		}

		if _, err := fhirxml.UnmarshalFHIRMap([]byte(payload)); err == nil {
			ht.Fatalf("expected UnmarshalFHIRMap to reject DTD/ENTITY payload:\n%s", payload)
		}
	}, hegel.WithTestCases(120))
}
