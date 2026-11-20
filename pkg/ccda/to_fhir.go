package ccda

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Template and LOINC section identifiers for C-CDA R2.1 CCD.
const (
	TemplateCCD             = "2.16.840.1.113883.10.20.22.1.2"
	SectionProblems         = "2.16.840.1.113883.10.20.22.2.5"
	SectionProblemsEntries  = "2.16.840.1.113883.10.20.22.2.5.1"
	SectionAllergies        = "2.16.840.1.113883.10.20.22.2.6"
	SectionAllergiesEntries = "2.16.840.1.113883.10.20.22.2.6.1"
	SectionMeds             = "2.16.840.1.113883.10.20.22.2.1"
	SectionMedsEntries      = "2.16.840.1.113883.10.20.22.2.1.1"
	SectionImmunizations    = "2.16.840.1.113883.10.20.22.2.2"
	SectionImmunEntries     = "2.16.840.1.113883.10.20.22.2.2.1"
	SectionResults          = "2.16.840.1.113883.10.20.22.2.3"
	SectionResultsEntries   = "2.16.840.1.113883.10.20.22.2.3.1"
	SectionVitals           = "2.16.840.1.113883.10.20.22.2.4"
	SectionVitalsEntries    = "2.16.840.1.113883.10.20.22.2.4.1"
	SectionProcedures       = "2.16.840.1.113883.10.20.22.2.7"
	SectionProcEntries      = "2.16.840.1.113883.10.20.22.2.7.1"
	SectionEncounters       = "2.16.840.1.113883.10.20.22.2.22"
	SectionEncEntries       = "2.16.840.1.113883.10.20.22.2.22.1"
)

// ImportOptions controls how C-CDA R2.1 XML is mapped into FHIR R4 resources.
type ImportOptions struct {
	Mode                     string // "transaction" (default) or "document"
	IncludeDocumentReference *bool  // Defaults to true
	Now                      time.Time
}

// ImportResult contains the resulting FHIR R4 Bundle and clinical summary metadata.
type ImportResult struct {
	Bundle              map[string]any
	PatientID           string
	CompositionID       string
	DocumentReferenceID string
	ResourceCounts      map[string]int
	SectionsParsed      []string
}

// ConvertToFHIR parses raw C-CDA R2.1 XML bytes and translates them into a FHIR R4 Bundle.
func ConvertToFHIR(rawXML []byte, opts ImportOptions) (*ImportResult, error) {
	doc, err := Parse(rawXML)
	if err != nil {
		return nil, err
	}
	return DocumentToFHIR(doc, rawXML, opts)
}

// DocumentToFHIR converts a parsed ClinicalDocument into a deterministic, idempotent
// FHIR R4 transaction or document Bundle.
func DocumentToFHIR(doc *ClinicalDocument, rawXML []byte, opts ImportOptions) (*ImportResult, error) {
	if doc == nil {
		return nil, ErrEmptyDocument
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	mode := strings.ToLower(strings.TrimSpace(opts.Mode))
	if mode == "" {
		mode = "transaction"
	}
	includeDocRef := true
	if opts.IncludeDocumentReference != nil {
		includeDocRef = *opts.IncludeDocumentReference
	}

	b := &fhirBuilder{
		entries:    make([]any, 0, 24),
		seenIDs:    make(map[string]int),
		counts:     make(map[string]int),
		isDocument: mode == "document",
	}

	docInstant := FormatCDAInstant(doc.EffectiveTime.Value, now)
	docIDKey := iiKey(doc.ID)
	if docIDKey == "" {
		docIDKey = deterministicToken("doc", doc.Title, docInstant, string(rawXML))
	}

	// Reserve slot 0 for Composition BEFORE inserting any author/custodian/clinical resources
	// when assembling a FHIR document Bundle.
	compID := deterministicID("comp", docIDKey)
	if b.isDocument {
		b.entries = append(b.entries, nil)
		b.seenIDs["Composition/"+compID] = 0
	}

	// 1. Custodian & Provider Organization + Author Practitioner(s)
	var custodianOrgID string
	if doc.Custodian != nil && doc.Custodian.AssignedCustodian != nil && doc.Custodian.AssignedCustodian.RepresentedCustodianOrganization != nil {
		custOrg := doc.Custodian.AssignedCustodian.RepresentedCustodianOrganization
		custodianOrgID, _ = b.addOrganization(custOrg, "custodian")
	}

	var authorRefs []any
	var primaryPractitionerRef string
	for idx, auth := range doc.Authors {
		if auth.AssignedAuthor == nil {
			continue
		}
		if auth.AssignedAuthor.AssignedPerson != nil {
			pID, pMap := mapPersonToPractitioner(auth.AssignedAuthor, idx+1)
			if pMap != nil {
				b.upsert("Practitioner", pID, pMap)
				ref := "Practitioner/" + pID
				authorRefs = append(authorRefs, map[string]any{"reference": ref})
				if primaryPractitionerRef == "" {
					primaryPractitionerRef = ref
				}
			}
		} else if auth.AssignedAuthor.RepresentedOrganization != nil {
			oID, oMap := mapOrganization(auth.AssignedAuthor.RepresentedOrganization, fmt.Sprintf("auth-%d", idx+1))
			if oMap != nil {
				b.upsert("Organization", oID, oMap)
				authorRefs = append(authorRefs, map[string]any{"reference": "Organization/" + oID})
			}
		}
	}
	if len(authorRefs) == 0 && custodianOrgID != "" {
		authorRefs = append(authorRefs, map[string]any{"reference": "Organization/" + custodianOrgID})
	}
	if len(authorRefs) == 0 {
		// Fallback Device author to satisfy HL7 FHIR R4 Composition.author 1..* cardinality
		devID := deterministicID("dev", "flint-ccda-importer")
		b.upsert("Device", devID, map[string]any{
			"resourceType": "Device",
			"id":           devID,
			"status":       "active",
			"deviceName": []any{
				map[string]any{"name": "Flint C-CDA R2.1 Importer", "type": "manufacturer-name"},
			},
		})
		authorRefs = append(authorRefs, map[string]any{"reference": "Device/" + devID})
	}

	// 2. Patient (recordTarget[0])
	if len(doc.RecordTargets) == 0 {
		return nil, fmt.Errorf("%w: missing recordTarget/patientRole", ErrInvalidCCDA)
	}
	pr := doc.RecordTargets[0].PatientRole
	if custodianOrgID == "" && pr.ProviderOrganization != nil {
		custodianOrgID, _ = b.addOrganization(pr.ProviderOrganization, "provider")
	}
	patID, patMap := mapPatientRole(pr, custodianOrgID, docIDKey)
	b.upsert("Patient", patID, patMap)

	// 4. Header EncompassingEncounter
	var headerEncID string
	if doc.ComponentOf != nil && doc.ComponentOf.EncompassingEncounter != nil {
		eID, eMap := mapEncompassingEncounter(doc.ComponentOf.EncompassingEncounter, patID, custodianOrgID, docIDKey)
		if eMap != nil {
			headerEncID = eID
			b.upsert("Encounter", eID, eMap)
		}
	}

	// 5. StructuredBody Clinical Sections
	var compSections []any
	var parsedSectionNames []string

	if doc.Component != nil && doc.Component.StructuredBody != nil {
		for sIdx, bodyComp := range doc.Component.StructuredBody.Components {
			sec := bodyComp.Section
			if sec == nil {
				continue
			}
			sectionKind := classifySection(sec)
			secTitle := firstStr(strings.TrimSpace(sec.Title), sectionDefaultTitle(sectionKind))
			parsedSectionNames = append(parsedSectionNames, sectionKind)

			var secEntryRefs []any
			if strings.TrimSpace(sec.NullFlavor) == "" {
				secEntryRefs = b.mapSectionEntries(sec, sectionKind, patID, headerEncID, custodianOrgID, docInstant, primaryPractitionerRef, sIdx+1)
			}

			secObj := map[string]any{
				"title": secTitle,
				"code":  mapCDToCodeableConcept(sec.Code, "http://loinc.org", sectionDefaultTitle(sectionKind)),
			}
			if nf := strings.TrimSpace(sec.NullFlavor); nf != "" && len(secEntryRefs) == 0 {
				secObj["emptyReason"] = map[string]any{
					"coding": []any{
						map[string]any{
							"system": "http://terminology.hl7.org/CodeSystem/list-empty-reason",
							"code":   "nilknown",
						},
					},
					"text": "NullFlavor " + nf,
				}
			} else if len(secEntryRefs) == 0 {
				secObj["emptyReason"] = map[string]any{
					"coding": []any{
						map[string]any{
							"system": "http://terminology.hl7.org/CodeSystem/list-empty-reason",
							"code":   "unavailable",
						},
					},
				}
			} else {
				secObj["entry"] = secEntryRefs
			}
			if sec.Text != nil && strings.TrimSpace(sec.Text.PlainText) != "" {
				secObj["text"] = map[string]any{
					"status": "generated",
					"div":    wrapXHTMLDiv(sec.Text.PlainText),
				}
			}
			compSections = append(compSections, secObj)
		}
	}

	// 6. Composition resource
	compCode := mapCDToCodeableConcept(doc.Code, "http://loinc.org", "Summarization of Episode Note")
	if len(compCode) == 0 || compCode["coding"] == nil {
		compCode = map[string]any{
			"coding": []any{
				map[string]any{
					"system":  "http://loinc.org",
					"code":    "34133-9",
					"display": "Summarization of Episode Note",
				},
			},
		}
	}
	compMap := map[string]any{
		"resourceType": "Composition",
		"id":           compID,
		"status":       "final",
		"type":         compCode,
		"subject":      map[string]any{"reference": "Patient/" + patID},
		"date":         docInstant,
		"author":       authorRefs,
		"title":        firstStr(strings.TrimSpace(doc.Title), "Continuity of Care Document"),
	}
	if custodianOrgID != "" {
		compMap["custodian"] = map[string]any{"reference": "Organization/" + custodianOrgID}
	}
	if headerEncID != "" {
		compMap["encounter"] = map[string]any{"reference": "Encounter/" + headerEncID}
	}
	if len(compSections) > 0 {
		compMap["section"] = compSections
	}
	if b.isDocument {
		b.entries[0] = map[string]any{
			"fullUrl":  "Composition/" + compID,
			"resource": compMap,
		}
		b.counts["Composition"]++
		b.targetRefs = append(b.targetRefs, "Composition/"+compID)
	} else {
		b.upsert("Composition", compID, compMap)
	}

	// 7. Optional Source DocumentReference carrying the original C-CDA R2.1 XML
	var docRefID string
	if includeDocRef && len(rawXML) > 0 && !b.isDocument {
		docRefID = deterministicID("docref", docIDKey)
		docRef := map[string]any{
			"resourceType": "DocumentReference",
			"id":           docRefID,
			"status":       "current",
			"type":         compCode,
			"subject":      map[string]any{"reference": "Patient/" + patID},
			"date":         docInstant,
			"author":       authorRefs,
			"content": []any{
				map[string]any{
					"attachment": map[string]any{
						"contentType": "application/cda+xml",
						"data":        base64.StdEncoding.EncodeToString(rawXML),
						"title":       firstStr(strings.TrimSpace(doc.Title), "Continuity of Care Document"),
					},
					"format": map[string]any{
						"system":  "http://ihe.net/fhir/ihe.formatcode.fhir/CodeSystem/formatcode",
						"code":    "urn:hl7-org:sdwg:ccda-structuredBody:2.1",
						"display": "For documents following C-CDA 2.1 constraints using a structured body",
					},
				},
			},
		}
		if custodianOrgID != "" {
			docRef["custodian"] = map[string]any{"reference": "Organization/" + custodianOrgID}
		}
		b.upsert("DocumentReference", docRefID, docRef)
	}

	// 8. Provenance resource (in transaction mode) linking imported clinical targets
	if !b.isDocument && len(b.targetRefs) > 0 {
		provID := deterministicID("prov", docIDKey)
		targets := make([]any, 0, len(b.targetRefs))
		for _, r := range b.targetRefs {
			targets = append(targets, map[string]any{"reference": r})
		}
		agentWho := authorRefs[0]
		prov := map[string]any{
			"resourceType": "Provenance",
			"id":           provID,
			"target":       targets,
			"recorded":     docInstant,
			"activity": map[string]any{
				"coding": []any{
					map[string]any{
						"system":  "http://terminology.hl7.org/CodeSystem/v3-DataOperation",
						"code":    "CREATE",
						"display": "C-CDA R2.1 Document Import",
					},
				},
			},
			"agent": []any{
				map[string]any{
					"type": map[string]any{
						"coding": []any{
							map[string]any{
								"system": "http://terminology.hl7.org/CodeSystem/provenance-participant-type",
								"code":   "assembler",
							},
						},
					},
					"who": agentWho,
				},
			},
		}
		b.upsertNoTarget("Provenance", provID, prov)
	}

	bundleType := "transaction"
	if b.isDocument {
		bundleType = "document"
	}
	bundle := map[string]any{
		"resourceType": "Bundle",
		"id":           deterministicID("bundle", mode, docIDKey),
		"type":         bundleType,
		"timestamp":    docInstant,
		"entry":        b.entries,
	}
	if b.isDocument {
		bundle["identifier"] = map[string]any{
			"system": "urn:flint:ccda:document",
			"value":  docIDKey,
		}
	}

	return &ImportResult{
		Bundle:              bundle,
		PatientID:           patID,
		CompositionID:       compID,
		DocumentReferenceID: docRefID,
		ResourceCounts:      b.counts,
		SectionsParsed:      parsedSectionNames,
	}, nil
}

type fhirBuilder struct {
	entries    []any
	seenIDs    map[string]int
	targetRefs []string
	counts     map[string]int
	isDocument bool
}

func (b *fhirBuilder) upsert(resType, id string, res map[string]any) {
	b.upsertInternal(resType, id, res, true)
}

func (b *fhirBuilder) upsertNoTarget(resType, id string, res map[string]any) {
	b.upsertInternal(resType, id, res, false)
}

func (b *fhirBuilder) upsertInternal(resType, id string, res map[string]any, track bool) {
	ref := resType + "/" + id
	entry := map[string]any{
		"fullUrl":  ref,
		"resource": res,
	}
	if !b.isDocument {
		entry["request"] = map[string]any{
			"method": "PUT",
			"url":    ref,
		}
	}
	if idx, exists := b.seenIDs[ref]; exists {
		b.entries[idx] = entry
		return
	}
	b.seenIDs[ref] = len(b.entries)
	b.entries = append(b.entries, entry)
	b.counts[resType]++
	if track {
		b.targetRefs = append(b.targetRefs, ref)
	}
}

func (b *fhirBuilder) addOrganization(org *Organization, prefix string) (string, map[string]any) {
	id, m := mapOrganization(org, prefix)
	if m != nil {
		b.upsert("Organization", id, m)
	}
	return id, m
}

func classifySection(sec *Section) string {
	for _, tpl := range sec.TemplateIDs {
		switch tpl.Root {
		case SectionProblems, SectionProblemsEntries:
			return "problems"
		case SectionAllergies, SectionAllergiesEntries:
			return "allergies"
		case SectionMeds, SectionMedsEntries:
			return "medications"
		case SectionImmunizations, SectionImmunEntries:
			return "immunizations"
		case SectionResults, SectionResultsEntries:
			return "results"
		case SectionVitals, SectionVitalsEntries:
			return "vitals"
		case SectionProcedures, SectionProcEntries:
			return "procedures"
		case SectionEncounters, SectionEncEntries:
			return "encounters"
		}
	}
	switch strings.TrimSpace(sec.Code.Code) {
	case "11450-4":
		return "problems"
	case "48765-2":
		return "allergies"
	case "10160-0":
		return "medications"
	case "11369-6":
		return "immunizations"
	case "30954-2":
		return "results"
	case "8716-3":
		return "vitals"
	case "47519-4":
		return "procedures"
	case "46240-8":
		return "encounters"
	default:
		return "other"
	}
}

func sectionDefaultTitle(kind string) string {
	switch kind {
	case "problems":
		return "Problems"
	case "allergies":
		return "Allergies and Intolerances"
	case "medications":
		return "Medications"
	case "immunizations":
		return "Immunizations"
	case "results":
		return "Results"
	case "vitals":
		return "Vital Signs"
	case "procedures":
		return "Procedures"
	case "encounters":
		return "Encounters"
	default:
		return "Clinical Section"
	}
}

func (b *fhirBuilder) mapSectionEntries(
	sec *Section,
	kind string,
	patID string,
	headerEncID string,
	custodianOrgID string,
	docInstant string,
	authorRef string,
	secSeq int,
) []any {
	var refs []any
	for eIdx, entry := range sec.Entries {
		seqKey := fmt.Sprintf("%d-%d", secSeq, eIdx+1)
		switch kind {
		case "problems":
			if entry.Act != nil && entry.Act.NullFlavor == "" {
				for rIdx, er := range entry.Act.EntryRelationships {
					if er.Observation != nil && er.Observation.NullFlavor == "" && er.Observation.NegationInd != "true" {
						cID, cMap := mapProblemObservation(er.Observation, patID, headerEncID, seqKey+fmt.Sprintf("-%d", rIdx+1))
						if cMap != nil {
							b.upsert("Condition", cID, cMap)
							refs = append(refs, map[string]any{"reference": "Condition/" + cID})
						}
					}
				}
			} else if entry.Observation != nil && entry.Observation.NullFlavor == "" {
				cID, cMap := mapProblemObservation(entry.Observation, patID, headerEncID, seqKey)
				if cMap != nil {
					b.upsert("Condition", cID, cMap)
					refs = append(refs, map[string]any{"reference": "Condition/" + cID})
				}
			}
		case "allergies":
			if entry.Act != nil && entry.Act.NullFlavor == "" {
				for rIdx, er := range entry.Act.EntryRelationships {
					if er.Observation != nil && er.Observation.NullFlavor == "" {
						aID, aMap := mapAllergyObservation(er.Observation, patID, seqKey+fmt.Sprintf("-%d", rIdx+1))
						if aMap != nil {
							b.upsert("AllergyIntolerance", aID, aMap)
							refs = append(refs, map[string]any{"reference": "AllergyIntolerance/" + aID})
						}
					}
				}
			} else if entry.Observation != nil && entry.Observation.NullFlavor == "" {
				aID, aMap := mapAllergyObservation(entry.Observation, patID, seqKey)
				if aMap != nil {
					b.upsert("AllergyIntolerance", aID, aMap)
					refs = append(refs, map[string]any{"reference": "AllergyIntolerance/" + aID})
				}
			}
		case "medications":
			if entry.SubstanceAdministration != nil && entry.SubstanceAdministration.NullFlavor == "" {
				mID, mMap := mapMedicationActivity(entry.SubstanceAdministration, patID, headerEncID, authorRef, docInstant, seqKey)
				if mMap != nil {
					b.upsert("MedicationRequest", mID, mMap)
					refs = append(refs, map[string]any{"reference": "MedicationRequest/" + mID})
				}
			}
		case "immunizations":
			if entry.SubstanceAdministration != nil && entry.SubstanceAdministration.NullFlavor == "" {
				iID, iMap, mfgID, mfgMap := mapImmunizationActivity(entry.SubstanceAdministration, patID, headerEncID, docInstant, seqKey)
				if mfgMap != nil {
					b.upsert("Organization", mfgID, mfgMap)
				}
				if iMap != nil {
					b.upsert("Immunization", iID, iMap)
					refs = append(refs, map[string]any{"reference": "Immunization/" + iID})
				}
			}
		case "results":
			if entry.Organizer != nil && entry.Organizer.NullFlavor == "" {
				var obsRefs []any
				var effDT string
				for cIdx, comp := range entry.Organizer.Components {
					if comp.Observation != nil && comp.Observation.NullFlavor == "" {
						oID, oMap := mapObservation(comp.Observation, patID, headerEncID, "laboratory", seqKey+fmt.Sprintf("-c%d", cIdx+1))
						if oMap != nil {
							b.upsert("Observation", oID, oMap)
							obsRefs = append(obsRefs, map[string]any{"reference": "Observation/" + oID})
							if effDT == "" {
								effDT, _ = oMap["effectiveDateTime"].(string)
							}
						}
					}
				}
				if len(obsRefs) > 0 || entry.Organizer.Code != nil {
					drID, drMap := mapResultOrganizer(entry.Organizer, patID, headerEncID, obsRefs, effDT, docInstant, seqKey)
					b.upsert("DiagnosticReport", drID, drMap)
					refs = append(refs, map[string]any{"reference": "DiagnosticReport/" + drID})
				}
			} else if entry.Observation != nil && entry.Observation.NullFlavor == "" {
				oID, oMap := mapObservation(entry.Observation, patID, headerEncID, "laboratory", seqKey)
				if oMap != nil {
					b.upsert("Observation", oID, oMap)
					refs = append(refs, map[string]any{"reference": "Observation/" + oID})
				}
			}
		case "vitals":
			if entry.Organizer != nil && entry.Organizer.NullFlavor == "" {
				orgEff := ""
				if entry.Organizer.EffectiveTime != nil {
					if dt, ok := ParseCDATimestamp(entry.Organizer.EffectiveTime.Value, false); ok {
						orgEff = dt
					}
				}
				for cIdx, comp := range entry.Organizer.Components {
					if comp.Observation != nil && comp.Observation.NullFlavor == "" {
						oID, oMap := mapObservation(comp.Observation, patID, headerEncID, "vital-signs", seqKey+fmt.Sprintf("-v%d", cIdx+1))
						if oMap != nil {
							if _, hasEff := oMap["effectiveDateTime"]; !hasEff && orgEff != "" {
								oMap["effectiveDateTime"] = orgEff
							}
							b.upsert("Observation", oID, oMap)
							refs = append(refs, map[string]any{"reference": "Observation/" + oID})
						}
					}
				}
			} else if entry.Observation != nil && entry.Observation.NullFlavor == "" {
				oID, oMap := mapObservation(entry.Observation, patID, headerEncID, "vital-signs", seqKey)
				if oMap != nil {
					b.upsert("Observation", oID, oMap)
					refs = append(refs, map[string]any{"reference": "Observation/" + oID})
				}
			}
		case "procedures":
			if entry.Procedure != nil && entry.Procedure.NullFlavor == "" {
				pID, pMap := mapProcedureAct(entry.Procedure, patID, headerEncID, seqKey)
				if pMap != nil {
					b.upsert("Procedure", pID, pMap)
					refs = append(refs, map[string]any{"reference": "Procedure/" + pID})
				}
			} else if entry.Act != nil && entry.Act.NullFlavor == "" {
				pID, pMap := mapGenericActAsProcedure(entry.Act, patID, headerEncID, seqKey)
				if pMap != nil {
					b.upsert("Procedure", pID, pMap)
					refs = append(refs, map[string]any{"reference": "Procedure/" + pID})
				}
			}
		case "encounters":
			if entry.Encounter != nil && entry.Encounter.NullFlavor == "" {
				eID, eMap := mapEncounterActivity(entry.Encounter, patID, custodianOrgID, seqKey)
				if eMap != nil {
					b.upsert("Encounter", eID, eMap)
					refs = append(refs, map[string]any{"reference": "Encounter/" + eID})
				}
			}
		}
	}
	return refs
}

func mapPatientRole(pr PatientRole, orgID string, fallbackKey string) (string, map[string]any) {
	var identifiers []any
	var primarySys, primaryVal string
	for _, id := range pr.IDs {
		val := strings.TrimSpace(id.Extension)
		root := strings.TrimSpace(id.Root)
		if val == "" && root == "" {
			continue
		}
		if val == "" {
			val = root
			root = "urn:ietf:rfc:3986"
		}
		sys := OIDToFHIRSystem(root)
		if primaryVal == "" {
			primarySys = sys
			primaryVal = val
		}
		idObj := map[string]any{
			"system": sys,
			"value":  val,
		}
		if id.Root == "2.16.840.1.113883.4.1" {
			idObj["type"] = map[string]any{
				"coding": []any{
					map[string]any{
						"system": "http://terminology.hl7.org/CodeSystem/v2-0203",
						"code":   "SS",
					},
				},
			}
		}
		identifiers = append(identifiers, idObj)
	}

	var patID string
	var birthDate string
	var names []any
	if pr.Patient != nil {
		if pr.Patient.BirthTime != nil {
			birthDate, _ = ParseCDATimestamp(pr.Patient.BirthTime.Value, true)
		}
		for _, en := range pr.Patient.Names {
			if hn := mapENToHumanName(en); hn != nil {
				names = append(names, hn)
			}
		}
	}
	if primaryVal != "" {
		patID = deterministicID("pat", primarySys, primaryVal)
	} else {
		patID = deterministicID("pat", birthDate, fallbackKey)
	}

	pat := map[string]any{
		"resourceType": "Patient",
		"id":           patID,
		"active":       true,
	}
	if len(identifiers) > 0 {
		pat["identifier"] = identifiers
	}
	if len(names) > 0 {
		pat["name"] = names
	}
	if birthDate != "" {
		pat["birthDate"] = birthDate
	}
	if addrs := mapADList(pr.Addrs); len(addrs) > 0 {
		pat["address"] = addrs
	}
	if tels := mapTELList(pr.Telecoms); len(tels) > 0 {
		pat["telecom"] = tels
	}
	if orgID != "" {
		pat["managingOrganization"] = map[string]any{"reference": "Organization/" + orgID}
	}

	if pr.Patient != nil {
		if pr.Patient.AdministrativeGender != nil {
			if g := mapCDAGender(pr.Patient.AdministrativeGender.Code); g != "" {
				pat["gender"] = g
			}
		}
		if pr.Patient.MaritalStatus != nil && strings.TrimSpace(pr.Patient.MaritalStatus.Code) != "" {
			pat["maritalStatus"] = mapCDToCodeableConcept(*pr.Patient.MaritalStatus, "http://terminology.hl7.org/CodeSystem/v3-MaritalStatus", "")
		}
		var exts []any
		var raceCodes []CD
		if pr.Patient.RaceCode != nil && (pr.Patient.RaceCode.Code != "" || pr.Patient.RaceCode.NullFlavor != "") {
			raceCodes = append(raceCodes, *pr.Patient.RaceCode)
		}
		raceCodes = append(raceCodes, pr.Patient.SDTCRaceCodes...)
		if raceExt := buildCDARaceExtension(raceCodes); raceExt != nil {
			exts = append(exts, raceExt)
		}

		var ethCodes []CD
		if pr.Patient.EthnicGroupCode != nil && (pr.Patient.EthnicGroupCode.Code != "" || pr.Patient.EthnicGroupCode.NullFlavor != "") {
			ethCodes = append(ethCodes, *pr.Patient.EthnicGroupCode)
		}
		ethCodes = append(ethCodes, pr.Patient.SDTCEthnicGroupCodes...)
		if ethExt := buildCDAEthnicityExtension(ethCodes); ethExt != nil {
			exts = append(exts, ethExt)
		}
		if len(exts) > 0 {
			pat["extension"] = exts
		}

		var comms []any
		for _, lc := range pr.Patient.LanguageCommunications {
			if lc.LanguageCode != nil && strings.TrimSpace(lc.LanguageCode.Code) != "" {
				cObj := map[string]any{
					"language": map[string]any{
						"coding": []any{
							map[string]any{
								"system": "urn:ietf:bcp:47",
								"code":   strings.TrimSpace(lc.LanguageCode.Code),
							},
						},
					},
				}
				if lc.PreferenceInd != nil && strings.EqualFold(lc.PreferenceInd.Value, "true") {
					cObj["preferred"] = true
				}
				comms = append(comms, cObj)
			}
		}
		if len(comms) > 0 {
			pat["communication"] = comms
		}

		if pr.Patient.SDTCDeceasedTime != nil && pr.Patient.SDTCDeceasedTime.Value != "" {
			if dt, ok := ParseCDATimestamp(pr.Patient.SDTCDeceasedTime.Value, false); ok {
				pat["deceasedDateTime"] = dt
			}
		} else if pr.Patient.SDTCDeceasedInd != nil && strings.EqualFold(pr.Patient.SDTCDeceasedInd.Value, "true") {
			pat["deceasedBoolean"] = true
		}
	}

	return patID, pat
}

func mapPersonToPractitioner(aa *AssignedAuthor, seq int) (string, map[string]any) {
	if aa == nil || aa.AssignedPerson == nil {
		return "", nil
	}
	var ids []any
	var keyParts []string
	for _, id := range aa.IDs {
		val := firstStr(id.Extension, id.Root)
		if val == "" {
			continue
		}
		sys := OIDToFHIRSystem(id.Root)
		if id.Extension == "" {
			sys = "urn:ietf:rfc:3986"
		}
		ids = append(ids, map[string]any{"system": sys, "value": val})
		keyParts = append(keyParts, sys+"|"+val)
	}
	var names []any
	for _, en := range aa.AssignedPerson.Names {
		if hn := mapENToHumanName(en); hn != nil {
			names = append(names, hn)
			if fam, _ := hn["family"].(string); fam != "" {
				keyParts = append(keyParts, fam)
			}
		}
	}
	if len(ids) == 0 && len(names) == 0 {
		return "", nil
	}
	if len(keyParts) == 0 {
		keyParts = append(keyParts, strconv.Itoa(seq))
	}
	pracID := deterministicID("prac", keyParts...)
	prac := map[string]any{
		"resourceType": "Practitioner",
		"id":           pracID,
		"active":       true,
	}
	if len(ids) > 0 {
		prac["identifier"] = ids
	}
	if len(names) > 0 {
		prac["name"] = names
	}
	if addrs := mapADList(aa.Addrs); len(addrs) > 0 {
		prac["address"] = addrs
	}
	if tels := mapTELList(aa.Telecoms); len(tels) > 0 {
		prac["telecom"] = tels
	}
	return pracID, prac
}

func mapOrganization(org *Organization, prefix string) (string, map[string]any) {
	if org == nil {
		return "", nil
	}
	var name string
	for _, n := range org.Names {
		if t := strings.TrimSpace(n); t != "" {
			name = t
			break
		}
	}
	var ids []any
	var keyParts []string
	for _, id := range org.IDs {
		val := firstStr(id.Extension, id.Root)
		if val == "" {
			continue
		}
		sys := OIDToFHIRSystem(id.Root)
		if id.Extension == "" {
			sys = "urn:ietf:rfc:3986"
		}
		ids = append(ids, map[string]any{"system": sys, "value": val})
		keyParts = append(keyParts, sys+"|"+val)
	}
	if name == "" && len(ids) == 0 {
		return "", nil
	}
	if name != "" {
		keyParts = append(keyParts, name)
	}
	orgID := deterministicID(prefix, keyParts...)
	out := map[string]any{
		"resourceType": "Organization",
		"id":           orgID,
		"active":       true,
		"name":         firstStr(name, "Organization "+orgID),
	}
	if len(ids) > 0 {
		out["identifier"] = ids
	}
	if addrs := mapADList(org.Addrs); len(addrs) > 0 {
		out["address"] = addrs
	}
	if tels := mapTELList(org.Telecoms); len(tels) > 0 {
		out["telecom"] = tels
	}
	return orgID, out
}

func mapEncompassingEncounter(ee *EncompassingEncounter, patID, orgID, fallbackKey string) (string, map[string]any) {
	if ee == nil {
		return "", nil
	}
	idKey := iiListKey(ee.IDs)
	if idKey == "" && ee.EffectiveTime != nil {
		idKey = firstStr(ee.EffectiveTime.Value)
		if ee.EffectiveTime.Low != nil {
			idKey += "|" + ee.EffectiveTime.Low.Value
		}
	}
	if idKey == "" {
		idKey = fallbackKey
	}
	encID := deterministicID("enc", patID, idKey)
	enc := map[string]any{
		"resourceType": "Encounter",
		"id":           encID,
		"status":       "finished",
		"class": map[string]any{
			"system":  "http://terminology.hl7.org/CodeSystem/v3-ActCode",
			"code":    "AMB",
			"display": "ambulatory",
		},
		"subject": map[string]any{"reference": "Patient/" + patID},
	}
	if ee.Code != nil && ee.Code.Code != "" {
		enc["type"] = []any{mapCDToCodeableConcept(*ee.Code, "http://www.ama-assn.org/go/cpt", "")}
	} else {
		enc["type"] = []any{
			map[string]any{
				"coding": []any{
					map[string]any{"system": "http://snomed.info/sct", "code": "185349003", "display": "Encounter for check up"},
				},
			},
		}
	}
	if p := mapTSToPeriod(ee.EffectiveTime); p != nil {
		enc["period"] = p
	}
	if orgID != "" {
		enc["serviceProvider"] = map[string]any{"reference": "Organization/" + orgID}
	}
	return encID, enc
}

func mapProblemObservation(obs *Observation, patID, encID, seqKey string) (string, map[string]any) {
	if obs == nil || len(obs.Values) == 0 {
		return "", nil
	}
	valCD := obs.Values[0]
	if valCD.NullFlavor != "" && valCD.Code == "" && len(valCD.Translations) == 0 {
		return "", nil
	}
	codeCC := mapCDToCodeableConcept(valCD, "http://snomed.info/sct", "")
	if len(codeCC) == 0 {
		return "", nil
	}
	idKey := firstStr(iiListKey(obs.IDs), valCD.Code, seqKey)
	cID := deterministicID("cond", patID, idKey, seqKey)

	clinStatus := "active"
	var abatement string
	var onset string
	if obs.EffectiveTime != nil {
		if obs.EffectiveTime.Low != nil && obs.EffectiveTime.Low.Value != "" {
			if dt, ok := ParseCDATimestamp(obs.EffectiveTime.Low.Value, false); ok {
				onset = dt
			}
		} else if obs.EffectiveTime.Value != "" {
			if dt, ok := ParseCDATimestamp(obs.EffectiveTime.Value, false); ok {
				onset = dt
			}
		}
		if obs.EffectiveTime.High != nil && obs.EffectiveTime.High.Value != "" && obs.EffectiveTime.High.NullFlavor == "" {
			if dt, ok := ParseCDATimestamp(obs.EffectiveTime.High.Value, false); ok {
				abatement = dt
				clinStatus = "resolved"
			}
		}
	}
	for _, er := range obs.EntryRelationships {
		if er.Observation != nil && er.Observation.Code.Code == "33999-4" && len(er.Observation.Values) > 0 {
			switch er.Observation.Values[0].Code {
			case "413322009":
				clinStatus = "resolved"
			case "73425007":
				clinStatus = "inactive"
			case "55561003":
				clinStatus = "active"
			}
		}
	}

	cond := map[string]any{
		"resourceType": "Condition",
		"id":           cID,
		"clinicalStatus": map[string]any{
			"coding": []any{
				map[string]any{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": clinStatus},
			},
		},
		"verificationStatus": map[string]any{
			"coding": []any{
				map[string]any{"system": "http://terminology.hl7.org/CodeSystem/condition-ver-status", "code": "confirmed"},
			},
		},
		"category": []any{
			map[string]any{
				"coding": []any{
					map[string]any{
						"system":  "http://terminology.hl7.org/CodeSystem/condition-category",
						"code":    "problem-list-item",
						"display": "Problem List Item",
					},
				},
			},
		},
		"code":    codeCC,
		"subject": map[string]any{"reference": "Patient/" + patID},
	}
	if encID != "" {
		cond["encounter"] = map[string]any{"reference": "Encounter/" + encID}
	}
	if onset != "" {
		cond["onsetDateTime"] = onset
	}
	if abatement != "" {
		cond["abatementDateTime"] = abatement
	}
	return cID, cond
}

func mapAllergyObservation(obs *Observation, patID, seqKey string) (string, map[string]any) {
	if obs == nil || obs.NegationInd == "true" {
		return "", nil
	}
	// Substance lives in participant[@typeCode='CSM']/participantRole/playingEntity/code
	var subCD *CD
	for _, p := range obs.Participants {
		if (p.TypeCode == "CSM" || p.TypeCode == "") && p.ParticipantRole != nil && p.ParticipantRole.PlayingEntity != nil && p.ParticipantRole.PlayingEntity.Code != nil {
			subCD = p.ParticipantRole.PlayingEntity.Code
			break
		}
	}
	if subCD == nil || (subCD.Code == "" && subCD.DisplayName == "" && subCD.NullFlavor != "") {
		return "", nil
	}
	codeCC := mapCDToCodeableConcept(*subCD, "http://www.nlm.nih.gov/research/umls/rxnorm", "")
	if len(codeCC) == 0 {
		return "", nil
	}

	idKey := firstStr(iiListKey(obs.IDs), subCD.Code, seqKey)
	alID := deterministicID("allergy", patID, idKey, seqKey)

	clinStatus := "active"
	if obs.EffectiveTime != nil && obs.EffectiveTime.High != nil && obs.EffectiveTime.High.Value != "" {
		clinStatus = "resolved"
	}

	al := map[string]any{
		"resourceType": "AllergyIntolerance",
		"id":           alID,
		"clinicalStatus": map[string]any{
			"coding": []any{
				map[string]any{"system": "http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical", "code": clinStatus},
			},
		},
		"verificationStatus": map[string]any{
			"coding": []any{
				map[string]any{"system": "http://terminology.hl7.org/CodeSystem/allergyintolerance-verification", "code": "confirmed"},
			},
		},
		"code":    codeCC,
		"patient": map[string]any{"reference": "Patient/" + patID},
	}

	// Category / intolerance type from observation.value
	if len(obs.Values) > 0 {
		switch obs.Values[0].Code {
		case "416098002", "419511003":
			al["category"] = []any{"medication"}
		case "414285001", "418471000":
			al["category"] = []any{"food"}
		case "419199007", "426232007":
			al["category"] = []any{"environment"}
		}
	}

	if obs.EffectiveTime != nil {
		rawOnset := obs.EffectiveTime.Value
		if rawOnset == "" && obs.EffectiveTime.Low != nil {
			rawOnset = obs.EffectiveTime.Low.Value
		}
		if dt, ok := ParseCDATimestamp(rawOnset, false); ok {
			al["onsetDateTime"] = dt
		}
	}

	// Nested Reaction Observations (template 2.16.840.1.113883.10.20.22.4.9) & Criticality (4.145)
	var reactions []any
	for _, er := range obs.EntryRelationships {
		if er.Observation == nil {
			continue
		}
		if hasTemplate(er.Observation.TemplateIDs, "2.16.840.1.113883.10.20.22.4.145") && len(er.Observation.Values) > 0 {
			switch er.Observation.Values[0].Code {
			case "CRITH":
				al["criticality"] = "high"
			case "CRITL":
				al["criticality"] = "low"
			}
			continue
		}
		if er.TypeCode == "MFST" || hasTemplate(er.Observation.TemplateIDs, "2.16.840.1.113883.10.20.22.4.9") {
			if len(er.Observation.Values) > 0 {
				rxn := map[string]any{
					"manifestation": []any{mapCDToCodeableConcept(er.Observation.Values[0], "http://snomed.info/sct", "")},
				}
				for _, subER := range er.Observation.EntryRelationships {
					if subER.Observation != nil && hasTemplate(subER.Observation.TemplateIDs, "2.16.840.1.113883.10.20.22.4.8") && len(subER.Observation.Values) > 0 {
						switch subER.Observation.Values[0].Code {
						case "24484000":
							rxn["severity"] = "severe"
							al["criticality"] = "high"
						case "6736007":
							rxn["severity"] = "moderate"
						case "255604002":
							rxn["severity"] = "mild"
						}
					}
				}
				reactions = append(reactions, rxn)
			}
		}
	}
	if len(reactions) > 0 {
		al["reaction"] = reactions
	}
	return alID, al
}

func mapMedicationActivity(sa *SubstanceAdministration, patID, encID, authorRef, docInstant, seqKey string) (string, map[string]any) {
	if sa == nil || sa.Consumable == nil || sa.Consumable.ManufacturedProduct == nil || sa.Consumable.ManufacturedProduct.ManufacturedMaterial == nil {
		return "", nil
	}
	mat := sa.Consumable.ManufacturedProduct.ManufacturedMaterial
	if mat.Code.NullFlavor != "" && mat.Code.Code == "" && len(mat.Code.Translations) == 0 {
		return "", nil
	}
	medCC := mapCDToCodeableConcept(mat.Code, "http://www.nlm.nih.gov/research/umls/rxnorm", mat.Name)
	if len(medCC) == 0 {
		return "", nil
	}

	idKey := firstStr(iiListKey(sa.IDs), mat.Code.Code, seqKey)
	mrID := deterministicID("medreq", patID, idKey, seqKey)

	status := "active"
	if sa.StatusCode != nil {
		switch strings.ToLower(strings.TrimSpace(sa.StatusCode.Code)) {
		case "completed":
			status = "completed"
		case "aborted", "cancelled":
			status = "cancelled"
		case "suspended", "held":
			status = "on-hold"
		}
	}

	authoredOn := docInstant
	var period map[string]any
	for _, ts := range sa.EffectiveTimes {
		if ts.Low != nil || ts.High != nil || ts.Value != "" {
			if p := mapTSToPeriod(&ts); p != nil && period == nil {
				period = p
				if startStr, _ := p["start"].(string); startStr != "" {
					authoredOn = startStr
				}
			}
		}
	}

	mr := map[string]any{
		"resourceType":              "MedicationRequest",
		"id":                        mrID,
		"status":                    status,
		"intent":                    "plan",
		"medicationCodeableConcept": medCC,
		"subject":                   map[string]any{"reference": "Patient/" + patID},
		"authoredOn":                authoredOn,
	}
	if encID != "" {
		mr["encounter"] = map[string]any{"reference": "Encounter/" + encID}
	}
	if authorRef != "" && strings.HasPrefix(authorRef, "Practitioner/") {
		mr["requester"] = map[string]any{"reference": authorRef}
	}

	dosage := map[string]any{}
	if sa.Text != nil && strings.TrimSpace(sa.Text.PlainText) != "" {
		dosage["text"] = strings.TrimSpace(sa.Text.PlainText)
	}
	if sa.RouteCode != nil && sa.RouteCode.Code != "" {
		dosage["route"] = mapCDToCodeableConcept(*sa.RouteCode, "http://terminology.hl7.org/CodeSystem/v3-RouteOfAdministration", "")
	}
	if sa.DoseQuantity != nil && sa.DoseQuantity.Value != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(sa.DoseQuantity.Value), 64); err == nil {
			dq := map[string]any{"value": f}
			if sa.DoseQuantity.Unit != "" {
				dq["unit"] = sa.DoseQuantity.Unit
				dq["system"] = "http://unitsofmeasure.org"
				dq["code"] = sa.DoseQuantity.Unit
			}
			dosage["doseAndRate"] = []any{map[string]any{"doseQuantity": dq}}
		}
	}
	if period != nil {
		dosage["timing"] = map[string]any{
			"repeat": map[string]any{"boundsPeriod": period},
		}
	}
	if len(dosage) > 0 {
		mr["dosageInstruction"] = []any{dosage}
	}
	return mrID, mr
}

func mapImmunizationActivity(sa *SubstanceAdministration, patID, encID, docInstant, seqKey string) (string, map[string]any, string, map[string]any) {
	if sa == nil || sa.Consumable == nil || sa.Consumable.ManufacturedProduct == nil || sa.Consumable.ManufacturedProduct.ManufacturedMaterial == nil {
		return "", nil, "", nil
	}
	mat := sa.Consumable.ManufacturedProduct.ManufacturedMaterial
	if mat.Code.NullFlavor != "" && mat.Code.Code == "" && len(mat.Code.Translations) == 0 {
		return "", nil, "", nil
	}
	vacCC := mapCDToCodeableConcept(mat.Code, "http://hl7.org/fhir/sid/cvx", mat.Name)
	if len(vacCC) == 0 {
		return "", nil, "", nil
	}

	idKey := firstStr(iiListKey(sa.IDs), mat.Code.Code, seqKey)
	immID := deterministicID("imm", patID, idKey, seqKey)

	status := "completed"
	if strings.EqualFold(sa.NegationInd, "true") {
		status = "not-done"
	}

	occ := docInstant[:10]
	for _, ts := range sa.EffectiveTimes {
		if dt, ok := ParseCDATimestamp(ts.Value, false); ok {
			occ = dt
			break
		}
		if ts.Low != nil {
			if dt, ok := ParseCDATimestamp(ts.Low.Value, false); ok {
				occ = dt
				break
			}
		}
	}

	imm := map[string]any{
		"resourceType":       "Immunization",
		"id":                 immID,
		"status":             status,
		"vaccineCode":        vacCC,
		"patient":            map[string]any{"reference": "Patient/" + patID},
		"occurrenceDateTime": occ,
		"primarySource":      true,
	}
	if encID != "" {
		imm["encounter"] = map[string]any{"reference": "Encounter/" + encID}
	}
	if strings.TrimSpace(mat.LotNumberText) != "" {
		imm["lotNumber"] = strings.TrimSpace(mat.LotNumberText)
	}
	if sa.RouteCode != nil && sa.RouteCode.Code != "" {
		imm["route"] = mapCDToCodeableConcept(*sa.RouteCode, "http://terminology.hl7.org/CodeSystem/v3-RouteOfAdministration", "")
	}
	if sa.DoseQuantity != nil && sa.DoseQuantity.Value != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(sa.DoseQuantity.Value), 64); err == nil {
			dq := map[string]any{"value": f}
			if sa.DoseQuantity.Unit != "" {
				dq["unit"] = sa.DoseQuantity.Unit
				dq["system"] = "http://unitsofmeasure.org"
				dq["code"] = sa.DoseQuantity.Unit
			}
			imm["doseQuantity"] = dq
		}
	}

	var mfgID string
	var mfgMap map[string]any
	if sa.Consumable.ManufacturedProduct.ManufacturerOrganization != nil {
		mfgID, mfgMap = mapOrganization(sa.Consumable.ManufacturedProduct.ManufacturerOrganization, "org-mfg")
		if mfgMap != nil {
			imm["manufacturer"] = map[string]any{
				"reference": "Organization/" + mfgID,
				"display":   mfgMap["name"],
			}
		}
	}

	return immID, imm, mfgID, mfgMap
}

func mapResultOrganizer(org *Organizer, patID, encID string, obsRefs []any, obsEff, docInstant, seqKey string) (string, map[string]any) {
	var codeCC map[string]any
	var codeKey string
	if org.Code != nil {
		codeCC = mapCDToCodeableConcept(*org.Code, "http://loinc.org", "Laboratory panel")
		codeKey = org.Code.Code
	}
	if len(codeCC) == 0 {
		codeCC = map[string]any{
			"coding": []any{map[string]any{"system": "http://loinc.org", "code": "11502-2", "display": "Laboratory report"}},
		}
	}
	idKey := firstStr(iiListKey(org.IDs), codeKey, seqKey)
	drID := deterministicID("dr", patID, idKey, seqKey)

	effDT := obsEff
	if org.EffectiveTime != nil {
		if dt, ok := ParseCDATimestamp(firstStr(org.EffectiveTime.Value, lowVal(org.EffectiveTime)), false); ok {
			effDT = dt
		}
	}
	dr := map[string]any{
		"resourceType": "DiagnosticReport",
		"id":           drID,
		"status":       "final",
		"category": []any{
			map[string]any{
				"coding": []any{
					map[string]any{"system": "http://terminology.hl7.org/CodeSystem/v2-0074", "code": "LAB", "display": "Laboratory"},
				},
			},
		},
		"code":    codeCC,
		"subject": map[string]any{"reference": "Patient/" + patID},
		"issued":  docInstant,
	}
	if effDT != "" {
		dr["effectiveDateTime"] = effDT
	}
	if encID != "" {
		dr["encounter"] = map[string]any{"reference": "Encounter/" + encID}
	}
	if len(obsRefs) > 0 {
		dr["result"] = obsRefs
	}
	return drID, dr
}

func mapObservation(obs *Observation, patID, encID, categoryCode, seqKey string) (string, map[string]any) {
	if obs == nil || (obs.Code.Code == "" && obs.Code.DisplayName == "" && obs.Code.NullFlavor != "") {
		return "", nil
	}
	codeCC := mapCDToCodeableConcept(obs.Code, "http://loinc.org", "Observation")
	idKey := firstStr(iiListKey(obs.IDs), obs.Code.Code, seqKey)
	obsID := deterministicID("obs", patID, categoryCode, idKey, seqKey)

	status := "final"
	if obs.StatusCode != nil {
		switch strings.ToLower(strings.TrimSpace(obs.StatusCode.Code)) {
		case "active", "preliminary":
			status = "preliminary"
		case "aborted", "cancelled":
			status = "cancelled"
		}
	}
	out := map[string]any{
		"resourceType": "Observation",
		"id":           obsID,
		"status":       status,
		"category": []any{
			map[string]any{
				"coding": []any{
					map[string]any{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": categoryCode},
				},
			},
		},
		"code":    codeCC,
		"subject": map[string]any{"reference": "Patient/" + patID},
	}
	if encID != "" {
		out["encounter"] = map[string]any{"reference": "Encounter/" + encID}
	}
	if obs.EffectiveTime != nil {
		rawEff := firstStr(obs.EffectiveTime.Value, lowVal(obs.EffectiveTime))
		if dt, ok := ParseCDATimestamp(rawEff, false); ok {
			out["effectiveDateTime"] = dt
		}
	}

	if len(obs.Values) > 0 {
		v := obs.Values[0]
		if v.NullFlavor != "" && v.Value == "" && v.Code == "" {
			out["dataAbsentReason"] = map[string]any{
				"coding": []any{
					map[string]any{
						"system": "http://terminology.hl7.org/CodeSystem/data-absent-reason",
						"code":   NullFlavorToDataAbsentReason(v.NullFlavor),
					},
				},
			}
		} else {
			applyObsCDAValue(out, v)
		}
	}

	var interps []any
	for _, ic := range obs.InterpretationCode {
		if ic.Code != "" {
			interps = append(interps, mapCDToCodeableConcept(ic, "http://terminology.hl7.org/CodeSystem/v3-ObservationInterpretation", ""))
		}
	}
	if len(interps) > 0 {
		out["interpretation"] = interps
	}

	var refRanges []any
	for _, rr := range obs.ReferenceRanges {
		if rr.ObservationRange == nil {
			continue
		}
		rrMap := map[string]any{}
		if rr.ObservationRange.Text != nil && strings.TrimSpace(rr.ObservationRange.Text.PlainText) != "" {
			rrMap["text"] = strings.TrimSpace(rr.ObservationRange.Text.PlainText)
		}
		if val := rr.ObservationRange.Value; val != nil {
			if val.Low != nil && val.Low.Value != "" {
				if f, err := strconv.ParseFloat(val.Low.Value, 64); err == nil {
					lq := map[string]any{"value": f}
					if val.Low.Unit != "" {
						lq["unit"] = val.Low.Unit
						lq["system"] = "http://unitsofmeasure.org"
						lq["code"] = val.Low.Unit
					}
					rrMap["low"] = lq
				}
			}
			if val.High != nil && val.High.Value != "" {
				if f, err := strconv.ParseFloat(val.High.Value, 64); err == nil {
					hq := map[string]any{"value": f}
					if val.High.Unit != "" {
						hq["unit"] = val.High.Unit
						hq["system"] = "http://unitsofmeasure.org"
						hq["code"] = val.High.Unit
					}
					rrMap["high"] = hq
				}
			}
		}
		if len(rrMap) > 0 {
			refRanges = append(refRanges, rrMap)
		}
	}
	if len(refRanges) > 0 {
		out["referenceRange"] = refRanges
	}
	return obsID, out
}

func applyObsCDAValue(out map[string]any, v CD) {
	xsi := strings.ToUpper(strings.TrimSpace(v.XSIType))
	switch {
	case xsi == "PQ" || (xsi == "" && v.Value != ""):
		if f, err := strconv.ParseFloat(strings.TrimSpace(v.Value), 64); err == nil {
			q := map[string]any{"value": f}
			if v.Unit != "" && v.Unit != "1" {
				q["unit"] = v.Unit
				q["system"] = "http://unitsofmeasure.org"
				q["code"] = v.Unit
			} else if v.Unit == "1" {
				q["system"] = "http://unitsofmeasure.org"
				q["code"] = "1"
			}
			out["valueQuantity"] = q
			return
		}
	case xsi == "INT":
		if n, err := strconv.Atoi(strings.TrimSpace(v.Value)); err == nil {
			out["valueInteger"] = n
			return
		}
	case xsi == "BL":
		if strings.EqualFold(v.Value, "true") {
			out["valueBoolean"] = true
			return
		}
		if strings.EqualFold(v.Value, "false") {
			out["valueBoolean"] = false
			return
		}
	case xsi == "CD" || xsi == "CE" || xsi == "CV" || v.Code != "":
		cc := mapCDToCodeableConcept(v, "http://snomed.info/sct", "")
		if len(cc) > 0 {
			out["valueCodeableConcept"] = cc
			return
		}
	}
	txt := firstStr(strings.TrimSpace(v.CharText), strings.TrimSpace(v.DisplayName), strings.TrimSpace(v.Value))
	if txt != "" {
		out["valueString"] = txt
	}
}

func mapProcedureAct(proc *Procedure, patID, encID, seqKey string) (string, map[string]any) {
	if proc == nil {
		return "", nil
	}
	codeCC := mapCDToCodeableConcept(proc.Code, "http://snomed.info/sct", "")
	if len(codeCC) == 0 {
		return "", nil
	}
	idKey := firstStr(iiListKey(proc.IDs), proc.Code.Code, seqKey)
	pID := deterministicID("proc", patID, idKey, seqKey)

	status := "completed"
	if proc.StatusCode != nil {
		switch strings.ToLower(strings.TrimSpace(proc.StatusCode.Code)) {
		case "active":
			status = "in-progress"
		case "aborted", "cancelled":
			status = "stopped"
		}
	}
	out := map[string]any{
		"resourceType": "Procedure",
		"id":           pID,
		"status":       status,
		"code":         codeCC,
		"subject":      map[string]any{"reference": "Patient/" + patID},
	}
	if encID != "" {
		out["encounter"] = map[string]any{"reference": "Encounter/" + encID}
	}
	if proc.EffectiveTime != nil {
		if dt, ok := ParseCDATimestamp(proc.EffectiveTime.Value, false); ok {
			out["performedDateTime"] = dt
		} else if p := mapTSToPeriod(proc.EffectiveTime); p != nil {
			out["performedPeriod"] = p
		}
	}
	var sites []any
	for _, ts := range proc.TargetSiteCodes {
		if sc := mapCDToCodeableConcept(ts, "http://snomed.info/sct", ""); len(sc) > 0 {
			sites = append(sites, sc)
		}
	}
	if len(sites) > 0 {
		out["bodySite"] = sites
	}
	return pID, out
}

func mapGenericActAsProcedure(act *Act, patID, encID, seqKey string) (string, map[string]any) {
	if act == nil || act.Code.Code == "" {
		return "", nil
	}
	pID := deterministicID("proc", patID, firstStr(iiListKey(act.IDs), act.Code.Code, seqKey), seqKey)
	out := map[string]any{
		"resourceType": "Procedure",
		"id":           pID,
		"status":       "completed",
		"code":         mapCDToCodeableConcept(act.Code, "http://snomed.info/sct", ""),
		"subject":      map[string]any{"reference": "Patient/" + patID},
	}
	if encID != "" {
		out["encounter"] = map[string]any{"reference": "Encounter/" + encID}
	}
	if act.EffectiveTime != nil {
		if dt, ok := ParseCDATimestamp(firstStr(act.EffectiveTime.Value, lowVal(act.EffectiveTime)), false); ok {
			out["performedDateTime"] = dt
		}
	}
	return pID, out
}

func mapEncounterActivity(ea *EncounterAct, patID, orgID, seqKey string) (string, map[string]any) {
	if ea == nil {
		return "", nil
	}
	var codeKey string
	if ea.Code != nil {
		codeKey = ea.Code.Code
	}
	idKey := firstStr(iiListKey(ea.IDs), codeKey, seqKey)
	eID := deterministicID("enc", patID, idKey, seqKey)

	enc := map[string]any{
		"resourceType": "Encounter",
		"id":           eID,
		"status":       "finished",
		"class": map[string]any{
			"system":  "http://terminology.hl7.org/CodeSystem/v3-ActCode",
			"code":    "AMB",
			"display": "ambulatory",
		},
		"subject": map[string]any{"reference": "Patient/" + patID},
	}
	if ea.Code != nil && (ea.Code.Code != "" || ea.Code.DisplayName != "") {
		enc["type"] = []any{mapCDToCodeableConcept(*ea.Code, "http://www.ama-assn.org/go/cpt", "")}
	}
	if p := mapTSToPeriod(ea.EffectiveTime); p != nil {
		enc["period"] = p
	}
	if orgID != "" {
		enc["serviceProvider"] = map[string]any{"reference": "Organization/" + orgID}
	}
	return eID, enc
}

func mapCDToCodeableConcept(cd CD, defaultSystem, fallbackText string) map[string]any {
	var codings []any
	var text string
	addCoding := func(c CD) {
		if strings.TrimSpace(c.Code) == "" || c.NullFlavor != "" {
			return
		}
		sys := defaultSystem
		if strings.TrimSpace(c.CodeSystem) != "" {
			sys = OIDToFHIRSystem(c.CodeSystem)
		}
		cm := map[string]any{
			"system": sys,
			"code":   strings.TrimSpace(c.Code),
		}
		if disp := strings.TrimSpace(c.DisplayName); disp != "" {
			cm["display"] = disp
			if text == "" {
				text = disp
			}
		}
		codings = append(codings, cm)
	}
	addCoding(cd)
	for _, tr := range cd.Translations {
		addCoding(tr)
	}
	if text == "" && cd.OriginalText != nil && strings.TrimSpace(cd.OriginalText.PlainText) != "" {
		text = strings.TrimSpace(cd.OriginalText.PlainText)
	}
	if text == "" {
		text = strings.TrimSpace(cd.DisplayName)
	}
	if text == "" && len(codings) == 0 {
		text = strings.TrimSpace(fallbackText)
	}
	out := map[string]any{}
	if len(codings) > 0 {
		out["coding"] = codings
	}
	if text != "" {
		out["text"] = text
	}
	return out
}

func mapENToHumanName(en EN) map[string]any {
	var family string
	for _, f := range en.Families {
		if t := strings.TrimSpace(f); t != "" {
			family = t
			break
		}
	}
	var givens []any
	for _, g := range en.Givens {
		if t := strings.TrimSpace(g); t != "" {
			givens = append(givens, t)
		}
	}
	if family == "" && len(givens) == 0 {
		return nil
	}
	hn := map[string]any{}
	switch strings.ToUpper(strings.TrimSpace(en.Use)) {
	case "L":
		hn["use"] = "official"
	case "P":
		hn["use"] = "nickname"
	}
	if family != "" {
		hn["family"] = family
	}
	if len(givens) > 0 {
		hn["given"] = givens
	}
	var prefixes []any
	for _, p := range en.Prefixes {
		if t := strings.TrimSpace(p); t != "" {
			prefixes = append(prefixes, t)
		}
	}
	if len(prefixes) > 0 {
		hn["prefix"] = prefixes
	}
	var suffixes []any
	for _, s := range en.Suffixes {
		if t := strings.TrimSpace(s); t != "" {
			suffixes = append(suffixes, t)
		}
	}
	if len(suffixes) > 0 {
		hn["suffix"] = suffixes
	}
	return hn
}

func mapADList(ads []AD) []any {
	var out []any
	for _, a := range ads {
		item := map[string]any{}
		var lines []any
		for _, l := range a.StreetAddressLine {
			if t := strings.TrimSpace(l); t != "" {
				lines = append(lines, t)
			}
		}
		if len(lines) > 0 {
			item["line"] = lines
		}
		if t := strings.TrimSpace(a.City); t != "" {
			item["city"] = t
		}
		if t := strings.TrimSpace(a.State); t != "" {
			item["state"] = t
		}
		if t := strings.TrimSpace(a.PostalCode); t != "" {
			item["postalCode"] = t
		}
		if t := strings.TrimSpace(a.Country); t != "" {
			item["country"] = t
		}
		if len(item) == 0 {
			continue
		}
		switch strings.ToUpper(strings.TrimSpace(a.Use)) {
		case "H", "HP":
			item["use"] = "home"
		case "WP":
			item["use"] = "work"
		}
		out = append(out, item)
	}
	return out
}

func mapTELList(tels []TEL) []any {
	var out []any
	for _, t := range tels {
		val := strings.TrimSpace(t.Value)
		if val == "" || t.NullFlavor != "" {
			continue
		}
		sys := "phone"
		lower := strings.ToLower(val)
		switch {
		case strings.HasPrefix(lower, "tel:"):
			val = strings.TrimSpace(val[4:])
		case strings.HasPrefix(lower, "mailto:"):
			sys = "email"
			val = strings.TrimSpace(val[7:])
		case strings.Contains(val, "@"):
			sys = "email"
		}
		if val == "" {
			continue
		}
		cp := map[string]any{"system": sys, "value": val}
		switch strings.ToUpper(strings.TrimSpace(t.Use)) {
		case "H", "HP":
			cp["use"] = "home"
		case "WP":
			cp["use"] = "work"
		case "MC":
			cp["use"] = "mobile"
		}
		out = append(out, cp)
	}
	return out
}

func buildCDARaceExtension(codes []CD) map[string]any {
	var sub []any
	var texts []string
	for _, c := range codes {
		code := strings.TrimSpace(c.Code)
		if code == "" {
			continue
		}
		disp := firstStr(strings.TrimSpace(c.DisplayName), code)
		if code == "1002-5" || code == "2028-9" || code == "2054-5" || code == "2076-8" || code == "2106-3" {
			sub = append(sub, map[string]any{
				"url": "ombCategory",
				"valueCoding": map[string]any{
					"system":  "urn:oid:2.16.840.1.113883.6.238",
					"code":    code,
					"display": disp,
				},
			})
		} else {
			sub = append(sub, map[string]any{
				"url": "detailed",
				"valueCoding": map[string]any{
					"system":  "urn:oid:2.16.840.1.113883.6.238",
					"code":    code,
					"display": disp,
				},
			})
		}
		texts = append(texts, disp)
	}
	if len(sub) == 0 {
		return nil
	}
	sub = append(sub, map[string]any{"url": "text", "valueString": strings.Join(texts, ", ")})
	return map[string]any{
		"url":       "http://hl7.org/fhir/us/core/StructureDefinition/us-core-race",
		"extension": sub,
	}
}

func buildCDAEthnicityExtension(codes []CD) map[string]any {
	var sub []any
	var texts []string
	for _, c := range codes {
		code := strings.TrimSpace(c.Code)
		if code == "" {
			continue
		}
		disp := firstStr(strings.TrimSpace(c.DisplayName), code)
		if code == "2135-2" || code == "2186-5" {
			sub = append(sub, map[string]any{
				"url": "ombCategory",
				"valueCoding": map[string]any{
					"system":  "urn:oid:2.16.840.1.113883.6.238",
					"code":    code,
					"display": disp,
				},
			})
		}
		texts = append(texts, disp)
	}
	if len(sub) == 0 {
		return nil
	}
	sub = append(sub, map[string]any{"url": "text", "valueString": strings.Join(texts, ", ")})
	return map[string]any{
		"url":       "http://hl7.org/fhir/us/core/StructureDefinition/us-core-ethnicity",
		"extension": sub,
	}
}

func mapCDAGender(code string) string {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "M":
		return "male"
	case "F":
		return "female"
	case "UN", "O", "OTHER":
		return "other"
	case "UNK":
		return "unknown"
	default:
		return ""
	}
}

func mapTSToPeriod(ts *TS) map[string]any {
	if ts == nil {
		return nil
	}
	p := map[string]any{}
	if ts.Low != nil {
		if dt, ok := ParseCDATimestamp(ts.Low.Value, false); ok {
			p["start"] = dt
		}
	} else if ts.Value != "" {
		if dt, ok := ParseCDATimestamp(ts.Value, false); ok {
			p["start"] = dt
		}
	}
	if ts.High != nil {
		if dt, ok := ParseCDATimestamp(ts.High.Value, false); ok {
			p["end"] = dt
		}
	}
	if len(p) == 0 {
		return nil
	}
	return p
}

func iiKey(id II) string {
	r := strings.TrimSpace(id.Root)
	e := strings.TrimSpace(id.Extension)
	if r == "" && e == "" {
		return ""
	}
	return r + "|" + e
}

func iiListKey(ids []II) string {
	for _, id := range ids {
		if k := iiKey(id); k != "" {
			return k
		}
	}
	return ""
}

func lowVal(ts *TS) string {
	if ts == nil || ts.Low == nil {
		return ""
	}
	return ts.Low.Value
}

func hasTemplate(tpls []II, root string) bool {
	for _, t := range tpls {
		if strings.TrimSpace(t.Root) == root {
			return true
		}
	}
	return false
}

func deterministicID(prefix string, parts ...string) string {
	return "ccda-" + prefix + "-" + deterministicToken(prefix, parts...)
}

func deterministicToken(prefix string, parts ...string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(prefix))
	for _, p := range parts {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(strings.TrimSpace(p)))
	}
	return hex.EncodeToString(h.Sum(nil)[:10])
}

func wrapXHTMLDiv(plain string) string {
	escaped := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(plain)
	return `<div xmlns="http://www.w3.org/1999/xhtml"><p>` + escaped + `</p></div>`
}

func firstStr(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}
