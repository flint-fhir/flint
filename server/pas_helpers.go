package server

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/flint-fhir/flint/pkg/fhirutil"
	"github.com/flint-fhir/flint/store"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

const (
	pasRequestBundleProfileURL         = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-request-bundle"
	pasResponseBundleProfileURL        = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-response-bundle"
	pasInquiryRequestBundleProfileURL  = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-inquiry-request-bundle"
	pasInquiryResponseBundleProfileURL = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-inquiry-response-bundle"
	pasClaimResponseProfileURL         = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claimresponse"
	pasClaimInquiryResponseProfileURL  = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claiminquiryresponse"
	pasCommRequestProfileURL           = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-communicationrequest"
	pasTaskProfileURL                  = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-task"
)

func resourceDeclaresProfilePrefix(res map[string]any, wantBase string) (matched bool, version string) {
	if res == nil {
		return false, ""
	}
	meta, _ := res["meta"].(map[string]any)
	if meta == nil {
		return false, ""
	}
	profs, _ := meta["profile"].([]any)
	for _, p := range profs {
		ps, _ := p.(string)
		ps = strings.TrimSpace(ps)
		base := ps
		ver := ""
		if idx := strings.IndexByte(ps, '|'); idx != -1 {
			base = strings.TrimSpace(ps[:idx])
			ver = strings.TrimSpace(ps[idx+1:])
		}
		if strings.EqualFold(base, wantBase) {
			return true, ver
		}
	}
	return false, ""
}

func detectPASDecisionScenario(bundleMap, claimMap map[string]any) (code, display string, isPended, isMustSupport, isRequestDAR bool) {
	var tokens []string
	addTok := func(s string) {
		if t := strings.ToUpper(strings.TrimSpace(s)); t != "" {
			tokens = append(tokens, t)
		}
	}
	if bundleMap != nil {
		if id, _ := bundleMap["id"].(string); id != "" {
			addTok(id)
		}
		if ident, ok := bundleMap["identifier"].(map[string]any); ok {
			if v, _ := ident["value"].(string); v != "" {
				addTok(v)
			}
		}
	}
	if claimMap != nil {
		if id, _ := claimMap["id"].(string); id != "" {
			addTok(id)
		}
		if idents, ok := claimMap["identifier"].([]any); ok {
			for _, rawID := range idents {
				if im, ok := rawID.(map[string]any); ok {
					if v, _ := im["value"].(string); v != "" {
						addTok(v)
					}
				}
			}
		}
	}
	joined := strings.Join(tokens, " ")
	switch {
	case strings.Contains(joined, "DENIAL") || strings.Contains(joined, "DENY"):
		return "A3", "Not Certified", false, false, false
	case strings.Contains(joined, "PENDED") || strings.Contains(joined, "PEND"):
		return "A4", "Pending", true, false, false
	default:
		ms := strings.Contains(joined, "MUST-SUPPORT") || strings.Contains(joined, "MUST_SUPPORT") || strings.Contains(joined, "MS-SUBMIT") || strings.Contains(joined, "MS-INQUIRE")
		dar := strings.Contains(joined, "DAR")
		return "A1", "Certified in total", false, ms, dar
	}
}

func extractFHIRBaseFromFullURL(fullURL string) string {
	u := strings.TrimRight(strings.TrimSpace(fullURL), "/")
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return ""
	}
	lastSlash := strings.LastIndexByte(u, '/')
	if lastSlash <= len("https://") {
		return ""
	}
	prefix := u[:lastSlash]
	prevSlash := strings.LastIndexByte(prefix, '/')
	if prevSlash <= len("https://") {
		return ""
	}
	return prefix[:prevSlash]
}

func resolvePASBundleEntryByRef(ref string, entries []map[string]any, rootURL string) (map[string]any, string) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, ""
	}
	candidates := []string{ref}
	if rootURL != "" && strings.Count(ref, "/") == 1 && !strings.HasPrefix(ref, "urn:") {
		candidates = append([]string{rootURL + "/" + ref}, candidates...)
	}
	for _, cand := range candidates {
		for _, e := range entries {
			fu, _ := e["fullUrl"].(string)
			if fu != "" && fu == cand {
				return e, fu
			}
		}
	}
	// Fallback match by ResourceType/id
	if strings.Count(ref, "/") == 1 && !strings.HasPrefix(ref, "urn:") {
		parts := strings.SplitN(ref, "/", 2)
		wantRT, wantID := parts[0], parts[1]
		for _, e := range entries {
			res, _ := e["resource"].(map[string]any)
			if res == nil {
				continue
			}
			rt, _ := res["resourceType"].(string)
			id, _ := res["id"].(string)
			if rt == wantRT && id == wantID {
				fu, _ := e["fullUrl"].(string)
				if fu == "" {
					if rootURL != "" {
						fu = rootURL + "/" + ref
					} else {
						fu = "urn:uuid:" + uuid.NewString()
					}
				}
				return e, fu
			}
		}
	}
	return nil, ref
}

func collectEchoedEntriesFromRequest(claimResp map[string]any, reqEntries []map[string]any, rootURL string) []map[string]any {
	seenFullURL := make(map[string]bool)
	var out []map[string]any

	var visitValue func(v any)
	visitValue = func(v any) {
		switch val := v.(type) {
		case map[string]any:
			if refStr, ok := val["reference"].(string); ok && strings.TrimSpace(refStr) != "" {
				if entry, resolvedFU := resolvePASBundleEntryByRef(refStr, reqEntries, rootURL); entry != nil && resolvedFU != "" {
					val["reference"] = resolvedFU
					if !seenFullURL[resolvedFU] {
						seenFullURL[resolvedFU] = true
						resMap, _ := entry["resource"].(map[string]any)
						if resMap != nil && resMap["resourceType"] != "Claim" {
							clonedEntry := map[string]any{
								"fullUrl":  resolvedFU,
								"resource": resMap,
							}
							out = append(out, clonedEntry)
							visitValue(resMap)
						}
					}
				}
				return
			}
			for k, child := range val {
				if k == "request" && val["resourceType"] == "ClaimResponse" {
					continue
				}
				visitValue(child)
			}
		case []any:
			for _, elem := range val {
				visitValue(elem)
			}
		case []map[string]any:
			for _, elem := range val {
				visitValue(elem)
			}
		}
	}

	visitValue(claimResp)
	return out
}

func extractOrganizationIdentifiersFromEntries(entries []map[string]any) []string {
	var out []string
	for _, e := range entries {
		res, _ := e["resource"].(map[string]any)
		if res == nil || res["resourceType"] != "Organization" {
			continue
		}
		idents, _ := res["identifier"].([]any)
		for _, rawID := range idents {
			if im, ok := rawID.(map[string]any); ok {
				if v, _ := im["value"].(string); strings.TrimSpace(v) != "" {
					out = append(out, strings.TrimSpace(v))
				}
			}
		}
	}
	return out
}

func buildPASReviewActionExt(code, display string) map[string]any {
	return map[string]any{
		"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewAction",
		"extension": []any{
			map[string]any{
				"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewActionCode",
				"valueCodeableConcept": map[string]any{
					"coding": []any{
						map[string]any{
							"system":  "https://codesystem.x12.org/005010/306",
							"code":    code,
							"display": display,
						},
					},
				},
			},
		},
	}
}

func itemHasCancelledModifier(itemMap map[string]any) bool {
	if itemMap == nil {
		return false
	}
	modExts, _ := itemMap["modifierExtension"].([]any)
	for _, rawE := range modExts {
		em, _ := rawE.(map[string]any)
		if em == nil {
			continue
		}
		u, _ := em["url"].(string)
		if strings.HasSuffix(u, "modifierextension-infoCancelled") {
			if b, ok := em["valueBoolean"].(bool); ok && b {
				return true
			}
		}
	}
	return false
}

func extractSubmittedItemAuthNumber(claimMap map[string]any) string {
	if claimMap == nil {
		return ""
	}
	items, _ := claimMap["item"].([]any)
	for _, rawI := range items {
		im, _ := rawI.(map[string]any)
		if im == nil {
			continue
		}
		exts, _ := im["extension"].([]any)
		for _, rawE := range exts {
			em, _ := rawE.(map[string]any)
			if em == nil {
				continue
			}
			u, _ := em["url"].(string)
			if strings.HasSuffix(u, "extension-authorizationNumber") {
				if v, _ := em["valueString"].(string); strings.TrimSpace(v) != "" {
					return strings.TrimSpace(v)
				}
			}
		}
	}
	return ""
}

// buildPASResponseBundle constructs a conformant HL7 Da Vinci PAS v2.2.1 Response Bundle
// (or Inquiry Response Bundle when operation == "inquire") from a submitted PAS Bundle and Claim.
func (s *Server) buildPASResponseBundle(
	tenant string,
	operation string, // "submit" or "inquire"
	reqBundle map[string]any,
	claimMap map[string]any,
	claimID string,
	overrideDecisionCode string,
	overrideDecisionDisp string,
) (respBundle map[string]any, respBundleID string, claimResp map[string]any, claimRespID string, isPended bool, orgIdents []string) {
	now := time.Now().UTC()
	nowISO := now.Format(time.RFC3339)
	issueDate := now.Format("2006-01-02")
	periodEnd := now.AddDate(0, 1, 0).Format("2006-01-02")

	var reqEntries []map[string]any
	var claimFullURL string
	var rootURL string
	if reqBundle != nil {
		if rawEntries, ok := reqBundle["entry"].([]any); ok {
			for _, re := range rawEntries {
				if em, ok := re.(map[string]any); ok {
					reqEntries = append(reqEntries, em)
					fu, _ := em["fullUrl"].(string)
					if rootURL == "" && strings.HasPrefix(fu, "http") {
						rootURL = extractFHIRBaseFromFullURL(fu)
					}
					if res, ok := em["resource"].(map[string]any); ok && res["resourceType"] == "Claim" && claimFullURL == "" {
						claimFullURL = fu
					}
				}
			}
		}
	}
	orgIdents = extractOrganizationIdentifiersFromEntries(reqEntries)

	decCode, decDisp, pendedFlag, isMS, isReqDAR := detectPASDecisionScenario(reqBundle, claimMap)
	if overrideDecisionCode != "" {
		decCode = overrideDecisionCode
		decDisp = overrideDecisionDisp
		pendedFlag = false
	}

	makeSynthFullURL := func(resType, id string) string {
		if rootURL != "" {
			return rootURL + "/" + resType + "/" + id
		}
		return "urn:uuid:" + id
	}

	respBundleID = uuid.NewString()
	claimRespID = uuid.NewString()
	crFullURL := makeSynthFullURL("ClaimResponse", claimRespID)

	bundleProfile := pasResponseBundleProfileURL
	crProfile := pasClaimResponseProfileURL
	if operation == "inquire" {
		bundleProfile = pasInquiryResponseBundleProfileURL
		crProfile = pasClaimInquiryResponseProfileURL
	}

	useStr, _ := claimMap["use"].(string)
	if useStr == "" {
		useStr = "preauthorization"
	}
	claimType := claimMap["type"]
	if claimType == nil {
		claimType = map[string]any{
			"coding": []any{
				map[string]any{
					"system": "http://terminology.hl7.org/CodeSystem/claim-type",
					"code":   "professional",
				},
			},
		}
	}

	cloneRef := func(raw any, fallbackDisplay string) map[string]any {
		if m, ok := raw.(map[string]any); ok {
			cp := make(map[string]any, len(m))
			for k, v := range m {
				cp[k] = v
			}
			return cp
		}
		return map[string]any{"display": fallbackDisplay}
	}

	patientRef := cloneRef(claimMap["patient"], "PAS Beneficiary")
	insurerRef := cloneRef(claimMap["insurer"], "PAS Insurer")
	requestorRef := cloneRef(claimMap["provider"], "PAS Requestor")

	// Resolve optional PractitionerRole / Practitioner in request entries so MS authorizedProvider echoes cleanly
	var pracRoleFullURL string
	for _, e := range reqEntries {
		if res, _ := e["resource"].(map[string]any); res != nil && res["resourceType"] == "PractitionerRole" {
			pracRoleFullURL, _ = e["fullUrl"].(string)
			break
		}
	}

	submittedAuthNum := extractSubmittedItemAuthNumber(claimMap)
	returnedAuthNum := "AUTH-PAS-1001"
	if submittedAuthNum != "" && strings.EqualFold(submittedAuthNum, returnedAuthNum) {
		returnedAuthNum = "AUTH-PAS-REPLACED-2002"
	} else if submittedAuthNum != "" && operation == "inquire" {
		returnedAuthNum = "AUTH-REPLACED-" + submittedAuthNum
	}

	var responseItems []any
	if items, ok := claimMap["item"].([]any); ok {
		for idx, rawItem := range items {
			im, ok := rawItem.(map[string]any)
			if !ok {
				continue
			}
			seq := idx + 1
			switch sVal := im["sequence"].(type) {
			case float64:
				if sVal > 0 {
					seq = int(sVal)
				}
			case int:
				if sVal > 0 {
					seq = sVal
				}
			}
			itemCode := decCode
			itemDisp := decDisp
			if itemHasCancelledModifier(im) && overrideDecisionCode == "" && !pendedFlag && decCode == "A1" {
				itemCode = "A3"
				itemDisp = "Not Certified"
			}

			itemExts := []any{
				map[string]any{
					"url":       "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemPreAuthIssueDate",
					"valueDate": issueDate,
				},
				map[string]any{
					"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemPreAuthPeriod",
					"valuePeriod": map[string]any{
						"start": issueDate,
						"end":   periodEnd,
					},
				},
			}
			if isMS || submittedAuthNum != "" {
				authProviderRef := map[string]any{"display": "Authorized Provider"}
				if pracRoleFullURL != "" {
					authProviderRef = map[string]any{"reference": pracRoleFullURL}
				}
				itemExts = append(itemExts,
					map[string]any{
						"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemTraceNumber",
						"valueIdentifier": map[string]any{
							"system": "http://flint.health/fhir/ITEM_TRACE_NUMBER",
							"value":  "TRACE-ITEM-1",
						},
					},
					map[string]any{
						"url":         "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-authorizationNumber",
						"valueString": returnedAuthNum,
					},
					map[string]any{
						"url":         "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-administrationReferenceNumber",
						"valueString": "ADMIN-REF-2001",
					},
					map[string]any{
						"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemRequestedServiceDate",
						"valuePeriod": map[string]any{
							"start": issueDate,
							"end":   periodEnd,
						},
					},
					map[string]any{
						"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemAuthorizedDetail",
						"extension": []any{
							map[string]any{
								"url": "productOrServiceCode",
								"valueCodeableConcept": map[string]any{
									"coding": []any{
										map[string]any{
											"system": "https://codesystem.x12.org/005010/1365",
											"code":   "3",
										},
									},
								},
							},
						},
					},
					map[string]any{
						"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemAuthorizedProvider",
						"extension": []any{
							map[string]any{
								"url":            "provider",
								"valueReference": authProviderRef,
							},
						},
					},
					map[string]any{
						"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-admissionDates",
						"valuePeriod": map[string]any{
							"start": issueDate,
						},
					},
					map[string]any{
						"url":       "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-dischargeDate",
						"valueDate": periodEnd,
					},
					map[string]any{
						"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-communicatedDiagnosis",
						"valueCodeableConcept": map[string]any{
							"coding": []any{
								map[string]any{
									"system": "http://hl7.org/fhir/sid/icd-10-cm",
									"code":   "G89.4",
								},
							},
						},
					},
				)
			}

			crItem := map[string]any{
				"extension":    itemExts,
				"itemSequence": seq,
				"adjudication": []any{
					map[string]any{
						"extension": []any{buildPASReviewActionExt(itemCode, itemDisp)},
						"category": map[string]any{
							"coding": []any{
								map[string]any{
									"system": "http://terminology.hl7.org/CodeSystem/adjudication",
									"code":   "submitted",
								},
							},
						},
					},
				},
			}
			if isMS {
				crItem["noteNumber"] = []any{1}
			}
			responseItems = append(responseItems, crItem)
		}
	}

	preAuthRefVal := "PA-" + strings.ToUpper(claimID)
	if claimID == "" {
		preAuthRefVal = "PA-" + strings.ToUpper(claimRespID[:8])
	}

	claimResp = map[string]any{
		"resourceType": "ClaimResponse",
		"id":           claimRespID,
		"meta": map[string]any{
			"profile": []any{crProfile},
		},
		"identifier": []any{
			map[string]any{
				"extension": []any{
					map[string]any{
						"url":         "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-identifierSubDepartment",
						"valueString": "SUBDEPT-01",
					},
					map[string]any{
						"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-identifierJurisdiction",
						"valueCodeableConcept": map[string]any{
							"coding": []any{
								map[string]any{
									"system": "https://www.usps.com/",
									"code":   "MA",
								},
							},
						},
					},
				},
				"system": "http://flint.health/fhir/PATIENT_EVENT_TRACE_NUMBER",
				"value":  preAuthRefVal,
			},
		},
		"status":     "active",
		"type":       claimType,
		"use":        useStr,
		"patient":    patientRef,
		"created":    nowISO,
		"insurer":    insurerRef,
		"requestor":  requestorRef,
		"outcome":    "complete",
		"preAuthRef": preAuthRefVal,
		"preAuthPeriod": map[string]any{
			"start": issueDate,
			"end":   periodEnd,
		},
	}

	if isReqDAR || (isMS && operation == "submit") {
		claimResp["request"] = map[string]any{
			"extension": []any{
				map[string]any{
					"url":       "http://hl7.org/fhir/StructureDefinition/data-absent-reason",
					"valueCode": "unknown",
				},
			},
		}
	} else if claimFullURL != "" {
		claimResp["request"] = map[string]any{"reference": claimFullURL}
	} else if claimID != "" {
		claimResp["request"] = map[string]any{"reference": "Claim/" + claimID}
	}

	if len(responseItems) > 0 {
		claimResp["item"] = responseItems
	}

	var extraEntries []map[string]any
	if isMS {
		authProvRef := map[string]any{"display": "Authorized Provider"}
		if pracRoleFullURL != "" {
			authProvRef = map[string]any{"reference": pracRoleFullURL}
		}
		claimResp["extension"] = []any{
			map[string]any{
				"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-TransmissionIdentifiers",
				"extension": []any{
					map[string]any{"url": "applicationSenderCode", "valueString": "FLINT-PAYER"},
					map[string]any{"url": "applicationReceiverCode", "valueString": "FLINT-PROV"},
				},
			},
			map[string]any{
				"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-claimResponseReviewer",
				"extension": []any{
					map[string]any{"url": "wasHumanReviewedFlag", "valueBoolean": true},
				},
			},
			map[string]any{
				"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemAuthorizedProvider",
				"extension": []any{
					map[string]any{"url": "provider", "valueReference": authProvRef},
				},
			},
		}
		claimResp["adjudication"] = []any{
			map[string]any{
				"extension": []any{buildPASReviewActionExt(decCode, decDisp)},
				"category": map[string]any{
					"coding": []any{
						map[string]any{
							"system": "http://terminology.hl7.org/CodeSystem/adjudication",
							"code":   "submitted",
						},
					},
				},
			},
		}
		claimResp["processNote"] = []any{
			map[string]any{
				"number": 1,
				"text":   "Prior authorization determination completed",
			},
		}
		claimResp["error"] = []any{
			map[string]any{
				"extension": []any{
					map[string]any{
						"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-errorFollowupAction",
						"valueCodeableConcept": map[string]any{
							"coding": []any{
								map[string]any{
									"system": "https://codesystem.x12.org/005010/889",
									"code":   "N",
								},
							},
						},
					},
					map[string]any{
						"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-errorElement",
						"extension": []any{
							map[string]any{
								"url":         "error",
								"valueString": "ClaimResponse.status",
							},
						},
					},
					map[string]any{
						"url":         "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-errorPath",
						"valueString": "ClaimResponse.status",
					},
				},
				"code": map[string]any{
					"coding": []any{
						map[string]any{
							"system": "https://codesystem.x12.org/005010/901",
							"code":   "04",
						},
					},
				},
			},
		}
		claimResp["addItem"] = []any{
			map[string]any{
				"extension": []any{
					map[string]any{
						"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemTraceNumber",
						"valueIdentifier": map[string]any{
							"system": "http://flint.health/fhir/ITEM_TRACE_NUMBER",
							"value":  "TRACE-ADD-1",
						},
					},
					map[string]any{
						"url":       "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemPreAuthIssueDate",
						"valueDate": issueDate,
					},
					map[string]any{
						"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemPreAuthPeriod",
						"valuePeriod": map[string]any{
							"start": issueDate,
							"end":   periodEnd,
						},
					},
					map[string]any{
						"url":         "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-authorizationNumber",
						"valueString": "AUTH-ADD-3001",
					},
					map[string]any{
						"url":         "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-administrationReferenceNumber",
						"valueString": "ADMIN-ADD-3001",
					},
					map[string]any{
						"url":          "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-epsdtIndicator",
						"valueBoolean": false,
					},
					map[string]any{
						"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-nursingHomeResidentialStatus",
						"valueCodeableConcept": map[string]any{
							"coding": []any{
								map[string]any{
									"system": "https://codesystem.x12.org/005010/1345",
									"code":   "2",
								},
							},
						},
					},
					map[string]any{
						"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-nursingHomeLevelOfCare",
						"valueCodeableConcept": map[string]any{
							"coding": []any{
								map[string]any{
									"system": "https://codesystem.x12.org/005010/1337",
									"code":   "2",
								},
							},
						},
					},
					map[string]any{
						"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-productOrServiceCodeEnd",
						"valueCodeableConcept": map[string]any{
							"coding": []any{
								map[string]any{
									"system": "https://codesystem.x12.org/005010/1365",
									"code":   "3",
								},
							},
						},
					},
					map[string]any{
						"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-revenueCode",
						"valueCodeableConcept": map[string]any{
							"coding": []any{
								map[string]any{
									"system": "https://www.nubc.org/CodeSystem/RevenueCodes",
									"code":   "0300",
								},
							},
						},
					},
					map[string]any{
						"url":          "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-revenueUnitRateLimit",
						"valueDecimal": 100.0,
					},
					map[string]any{
						"url":            "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-requestedService",
						"valueReference": map[string]any{"display": "Requested Consultation Service"},
					},
					map[string]any{
						"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-serviceItemRequestType",
						"valueCodeableConcept": map[string]any{
							"coding": []any{
								map[string]any{
									"system":  "https://codesystem.x12.org/005010/1525",
									"code":    "SC",
									"display": "Specialty Care Review",
								},
							},
						},
					},
					map[string]any{
						"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-certificationType",
						"valueCodeableConcept": map[string]any{
							"coding": []any{
								map[string]any{
									"system":  "https://codesystem.x12.org/005010/1322",
									"code":    "I",
									"display": "Initial",
								},
							},
						},
					},
					map[string]any{
						"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-admissionDates",
						"valuePeriod": map[string]any{
							"start": issueDate,
						},
					},
					map[string]any{
						"url":       "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-dischargeDate",
						"valueDate": periodEnd,
					},
				},
				"itemSequence": []any{1},
				"provider": []any{
					map[string]any{
						"extension": []any{
							map[string]any{
								"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-authorizedProviderType",
								"valueCodeableConcept": map[string]any{
									"coding": []any{
										map[string]any{
											"system": "https://codesystem.x12.org/005010/98",
											"code":   "IL",
										},
									},
								},
							},
						},
						"display": "Authorized AddItem Provider",
					},
				},
				"productOrService": map[string]any{
					"coding": []any{
						map[string]any{
							"system": "https://codesystem.x12.org/005010/1365",
							"code":   "3",
						},
					},
				},
				"modifier": []any{
					map[string]any{
						"coding": []any{
							map[string]any{
								"system": "http://www.ama-assn.org/go/cpt",
								"code":   "25",
							},
						},
					},
				},
				"servicedDate": issueDate,
				"locationCodeableConcept": map[string]any{
					"coding": []any{
						map[string]any{
							"system": "https://www.cms.gov/Medicare/Coding/place-of-service-codes/Place_of_Service_Code_Set",
							"code":   "11",
						},
					},
				},
				"quantity": map[string]any{
					"value":  1,
					"unit":   "unit",
					"system": "https://codesystem.x12.org/005010/673",
					"code":   "UN",
				},
				"unitPrice": map[string]any{
					"value":    100.0,
					"currency": "USD",
				},
				"adjudication": []any{
					map[string]any{
						"extension": []any{buildPASReviewActionExt("A1", "Certified in total")},
						"category": map[string]any{
							"coding": []any{
								map[string]any{
									"system": "http://terminology.hl7.org/CodeSystem/adjudication",
									"code":   "submitted",
								},
							},
						},
					},
				},
			},
		}

		if operation == "submit" {
			commID := uuid.NewString()
			commFullURL := makeSynthFullURL("CommunicationRequest", commID)
			claimResp["communicationRequest"] = []any{
				map[string]any{"reference": commFullURL},
			}
			commRes := map[string]any{
				"resourceType": "CommunicationRequest",
				"id":           commID,
				"meta": map[string]any{
					"profile": []any{pasCommRequestProfileURL},
				},
				"extension": []any{
					map[string]any{
						"url":              "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-serviceLineNumber",
						"valuePositiveInt": 1,
					},
				},
				"identifier": []any{
					map[string]any{
						"system": "http://flint.health/fhir/comm-req-id",
						"value":  "COMM-" + commID[:8],
					},
				},
				"status": "active",
				"category": []any{
					map[string]any{
						"coding": []any{
							map[string]any{
								"system":  "https://codesystem.x12.org/005010/755",
								"code":    "15",
								"display": "Justification for Admissions",
							},
						},
					},
				},
				"medium": []any{
					map[string]any{
						"coding": []any{
							map[string]any{
								"system": "http://terminology.hl7.org/CodeSystem/v3-ParticipationMode",
								"code":   "WRITTEN",
							},
						},
					},
				},
				"subject": map[string]any{"display": "PAS Beneficiary"},
				"payload": []any{
					map[string]any{
						"extension": []any{
							map[string]any{
								"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-communicatedDiagnosis",
								"valueCodeableConcept": map[string]any{
									"coding": []any{
										map[string]any{
											"system": "http://hl7.org/fhir/sid/icd-10-cm",
											"code":   "G89.4",
										},
									},
								},
							},
							map[string]any{
								"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-contentModifier",
								"valueCodeableConcept": map[string]any{
									"coding": []any{
										map[string]any{
											"system": "http://loinc.org",
											"code":   "18804-5",
										},
									},
								},
							},
						},
						"contentString": "Supporting clinical note requested",
					},
				},
				"requester": map[string]any{"display": "PAS Insurer"},
				"recipient": []any{map[string]any{"display": "PAS Requestor"}},
				"sender":    map[string]any{"display": "PAS Insurer"},
			}
			extraEntries = append(extraEntries, map[string]any{
				"fullUrl":  commFullURL,
				"resource": commRes,
			})
		}

		taskID := uuid.NewString()
		taskFullURL := makeSynthFullURL("Task", taskID)
		orgNPI := "8189991234"
		if len(orgIdents) > 0 {
			orgNPI = orgIdents[0]
		}
		taskRes := map[string]any{
			"resourceType": "Task",
			"id":           taskID,
			"meta": map[string]any{
				"profile": []any{pasTaskProfileURL},
			},
			"identifier": []any{
				map[string]any{
					"system": "http://flint.health/fhir/TASK_ID",
					"value":  "TASK-" + taskID[:8],
				},
			},
			"status": "requested",
			"statusReason": map[string]any{
				"coding": []any{
					map[string]any{
						"system": "http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes",
						"code":   "priorAuthorization",
					},
				},
			},
			"intent": "order",
			"code": map[string]any{
				"coding": []any{
					map[string]any{
						"system": "http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes",
						"code":   "attachment-request-code",
					},
				},
			},
			"for": map[string]any{"display": "PAS Beneficiary"},
			"requester": map[string]any{
				"identifier": map[string]any{
					"system": "http://hl7.org/fhir/sid/us-npi",
					"value":  "8383066098",
				},
			},
			"owner": map[string]any{
				"identifier": map[string]any{
					"system": "http://hl7.org/fhir/sid/us-npi",
					"value":  orgNPI,
				},
			},
			"reasonCode": map[string]any{
				"coding": []any{
					map[string]any{
						"system": "http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes",
						"code":   "priorAuthorization",
					},
				},
			},
			"reasonReference": map[string]any{"display": "Prior Authorization Claim"},
			"restriction": map[string]any{
				"period": map[string]any{
					"start": issueDate,
					"end":   periodEnd,
				},
			},
			"input": []any{
				map[string]any{
					"type": map[string]any{
						"coding": []any{
							map[string]any{
								"system": "http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes",
								"code":   "payer-url",
							},
						},
					},
					"valueUrl": s.resolveTenantFHIRBaseURL(tenant),
				},
				map[string]any{
					"extension": []any{
						map[string]any{
							"url":              "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-serviceLineNumber",
							"valuePositiveInt": 1,
						},
						map[string]any{
							"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-contentModifier",
							"valueCodeableConcept": map[string]any{
								"coding": []any{
									map[string]any{
										"system": "http://loinc.org",
										"code":   "18804-5",
									},
								},
							},
						},
					},
					"type": map[string]any{
						"coding": []any{
							map[string]any{
								"system": "http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes",
								"code":   "attachments-needed",
							},
						},
					},
					"valueCodeableConcept": map[string]any{
						"coding": []any{
							map[string]any{
								"system": "http://loinc.org",
								"code":   "28570-0",
							},
						},
					},
				},
				map[string]any{
					"extension": []any{
						map[string]any{
							"url":              "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-serviceLineNumber",
							"valuePositiveInt": 1,
						},
					},
					"type": map[string]any{
						"coding": []any{
							map[string]any{
								"system": "http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes",
								"code":   "questionnaire-context",
							},
						},
					},
					"valueString": "http://flint.health/fhir/Questionnaire/pas-prior-auth",
				},
			},
		}
		extraEntries = append(extraEntries, map[string]any{
			"fullUrl":  taskFullURL,
			"resource": taskRes,
		})
	}

	echoedEntries := collectEchoedEntriesFromRequest(claimResp, reqEntries, rootURL)

	allEntries := make([]any, 0, 1+len(extraEntries)+len(echoedEntries))
	allEntries = append(allEntries, map[string]any{
		"fullUrl":  crFullURL,
		"resource": claimResp,
	})
	for _, ee := range extraEntries {
		allEntries = append(allEntries, ee)
	}
	for _, ec := range echoedEntries {
		allEntries = append(allEntries, ec)
	}

	respBundle = map[string]any{
		"resourceType": "Bundle",
		"id":           respBundleID,
		"meta": map[string]any{
			"profile": []any{bundleProfile},
		},
		"identifier": map[string]any{
			"system": "urn:ietf:rfc:3986",
			"value":  "urn:uuid:" + respBundleID,
		},
		"type":      "collection",
		"timestamp": nowISO,
		"entry":     allEntries,
	}

	return respBundle, respBundleID, claimResp, claimRespID, pendedFlag, orgIdents
}

// schedulePASPendedFinalization asynchronously finalizes an A4 Pending prior authorization
// into an A1 Approved PAS Response Bundle and dispatches a PASSubscriptionTopic rest-hook notification.
func (s *Server) schedulePASPendedFinalization(
	reqCtx context.Context,
	tenant string,
	reqBundle map[string]any,
	claimMap map[string]any,
	claimID string,
) {
	finalBundle, finalBundleID, finalCR, finalCRID, _, orgIdents := s.buildPASResponseBundle(
		tenant, "submit", reqBundle, claimMap, claimID, "A1", "Certified in total",
	)
	s.startBackgroundWorker(func() {
		asyncCtx, cancel := s.deriveBackgroundContext(reqCtx)
		defer cancel()
		timer := time.NewTimer(450 * time.Millisecond)
		select {
		case <-asyncCtx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if s.store != nil && finalCR != nil {
			if md, ok := s.protoRegistry["ClaimResponse"]; ok {
				_, _ = s.persistSystemMapResource(asyncCtx, tenant, "ClaimResponse", finalCRID, finalCR, md)
			}
		}
		s.dispatchPASBundleNotification(asyncCtx, tenant, finalBundleID, finalCRID, finalBundle, orgIdents)
	})
}

func (s *Server) persistSystemMapResource(ctx context.Context, tenant, resType, resID string, resMap map[string]any, md protoreflect.MessageDescriptor) (string, error) {
	if s.store == nil || md == nil {
		return resID, nil
	}
	if resID == "" {
		resID = generateID()
	}
	resMap["id"] = resID
	rawJSON, err := json.Marshal(resMap)
	if err != nil {
		return "", err
	}
	normBody, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, rawJSON)
	if err != nil {
		return "", err
	}
	msg := dynamicpb.NewMessage(md)
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(normBody, msg); err != nil {
		return "", err
	}
	setProtoResourceID(msg, resID)
	protoBytes, err := proto.Marshal(msg)
	if err != nil {
		return "", err
	}
	var idx *store.SearchIndexes
	if extractor, ok := s.extractors[resType]; ok {
		idx, _ = extractor(tenant, resID, protoBytes)
	}
	_, err = s.store.WriteResourceWithMeta(ctx, store.ResourceInput{
		TenantID:      tenant,
		ResType:       resType,
		ResID:         resID,
		ResourceProto: protoBytes,
		SearchIndexes: idx,
	})
	return resID, err
}
