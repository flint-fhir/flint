package hl7v2

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrUnsupportedMessageType indicates an HL7 v2 message type outside the supported clinical set.
var ErrUnsupportedMessageType = errors.New("hl7v2: unsupported message type")

// MapOptions configures deterministic HL7 v2 -> FHIR R4 transaction Bundle mapping.
type MapOptions struct {
	Tenant              string
	DefaultSystemPrefix string
	Now                 time.Time
}

// MapResult contains the translated FHIR R4 transaction Bundle and extracted routing metadata.
type MapResult struct {
	Bundle         map[string]any
	MessageType    string
	TriggerEvent   string
	ControlID      string
	PatientID      string
	EncounterID    string
	ResourceCounts map[string]int
}

// MapToFHIRBundle translates a parsed HL7 v2.x message (ADT^A01/A03/A04/A08, ORU^R01, VXU^V04, ORM^O01)
// into an atomic, deterministic, idempotent HL7 FHIR R4 transaction Bundle with Provenance lineage.
func MapToFHIRBundle(msg *Message, opts MapOptions) (*MapResult, error) {
	if msg == nil || len(msg.Segments) == 0 {
		return nil, ErrEmptyMessage
	}
	code, trigger, _ := msg.MessageType()
	code = strings.ToUpper(strings.TrimSpace(code))
	trigger = strings.ToUpper(strings.TrimSpace(trigger))
	if code == "" {
		return nil, fmt.Errorf("%w: missing MSH-9 message code", ErrUnsupportedMessageType)
	}
	switch code {
	case "ADT", "ORU", "VXU", "ORM", "OMG", "OML":
	default:
		return nil, fmt.Errorf("%w: %s^%s", ErrUnsupportedMessageType, code, trigger)
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	sysPrefix := strings.TrimRight(strings.TrimSpace(opts.DefaultSystemPrefix), "/")
	if sysPrefix == "" {
		sysPrefix = "urn:flint:hl7v2"
	}

	b := &bundleBuilder{
		sysPrefix: sysPrefix,
		now:       now,
		entries:   make([]any, 0, 16),
		seenIDs:   make(map[string]int),
		counts:    make(map[string]int),
	}

	msgKey := msg.ControlID()
	if msgKey == "" {
		msgKey = deterministicToken("msg", msg.Serialize())
	}

	// 1. Sending Facility Organization (MSH-4)
	var orgID string
	if facName := strings.TrimSpace(msg.ValFromMSH(4, 1)); facName != "" {
		facHD := strings.TrimSpace(msg.ValFromMSH(4, 2))
		orgID = deterministicID("org", facHD, facName)
		org := map[string]any{
			"resourceType": "Organization",
			"id":           orgID,
			"active":       true,
			"name":         facName,
		}
		if facHD != "" {
			org["identifier"] = []any{
				map[string]any{
					"system": normalizeAssigningAuthority(facHD, sysPrefix+"/facility"),
					"value":  facHD,
				},
			}
		}
		b.upsert("Organization", orgID, org)
	}

	// 2. Patient (PID + PD1 + NK1)
	pid := msg.Segment("PID")
	if pid == nil {
		return nil, errors.New("hl7v2: missing required PID segment")
	}
	patID, patMap := b.mapPatient(pid, msg.SegmentsByName("NK1"), orgID, msgKey)
	b.upsert("Patient", patID, patMap)

	// Also emit RelatedPerson resources for NK1 segments with names
	for idx, nk1 := range msg.SegmentsByName("NK1") {
		if rpID, rpMap := b.mapRelatedPerson(nk1, patID, idx+1); rpMap != nil {
			b.upsert("RelatedPerson", rpID, rpMap)
		}
	}

	// 3. Encounter (PV1 + EVN)
	var encID string
	if pv1 := msg.Segment("PV1"); pv1 != nil {
		encID = b.mapVisit(pv1, msg.Segment("EVN"), code, trigger, patID, orgID, msgKey)
	} else if code == "ADT" {
		// Synthesize an Encounter even when PV1 is minimal/absent on ADT events
		encID = b.mapSyntheticADTEncounter(msg.Segment("EVN"), trigger, patID, orgID, msgKey)
	}

	// 4. Allergies (AL1)
	for idx, al1 := range msg.SegmentsByName("AL1") {
		if alID, alMap := b.mapAllergy(al1, patID, idx+1); alMap != nil {
			b.upsert("AllergyIntolerance", alID, alMap)
		}
	}

	// 5. Diagnoses / Problems (DG1)
	var condRefs []any
	for idx, dg1 := range msg.SegmentsByName("DG1") {
		if cID, cMap := b.mapCondition(dg1, patID, encID, idx+1); cMap != nil {
			b.upsert("Condition", cID, cMap)
			condRefs = append(condRefs, map[string]any{"reference": "Condition/" + cID})
		}
	}

	// 6. Message-family clinical payload
	msgTimestamp := formatHL7InstantOrFallback(msg.MessageDateTime(), now)
	switch code {
	case "ORU":
		b.mapORUGroups(msg, patID, encID, msgKey, msgTimestamp)
	case "VXU":
		b.mapVXUGroups(msg, patID, encID, msgKey)
	case "ORM", "OMG", "OML":
		b.mapORMOrders(msg, patID, encID, condRefs, msgKey, msgTimestamp)
	}

	// 7. Provenance lineage resource linking every clinical resource in this message
	provTargets := make([]any, 0, len(b.targetRefs))
	for _, ref := range b.targetRefs {
		provTargets = append(provTargets, map[string]any{"reference": ref})
	}
	if len(provTargets) > 0 {
		provID := deterministicID("prov", msg.SendingApplication(), msg.SendingFacility(), code, trigger, msgKey)
		agentWho := map[string]any{"display": firstNonEmptyStr(msg.SendingApplication(), msg.SendingFacility(), "HL7v2 Sender")}
		if orgID != "" {
			agentWho["reference"] = "Organization/" + orgID
		}
		msgTypeLabel := code
		if trigger != "" {
			msgTypeLabel = code + "^" + trigger
		}
		prov := map[string]any{
			"resourceType": "Provenance",
			"id":           provID,
			"target":       provTargets,
			"recorded":     msgTimestamp,
			"activity": map[string]any{
				"coding": []any{
					map[string]any{
						"system":  "http://terminology.hl7.org/CodeSystem/v2-0003",
						"code":    firstNonEmptyStr(trigger, code),
						"display": "HL7 v2 " + msgTypeLabel,
					},
				},
				"text": fmt.Sprintf("HL7 v2 %s message (controlId=%s)", msgTypeLabel, msgKey),
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

	msgTypeDisplay := code
	if trigger != "" {
		msgTypeDisplay = code + "^" + trigger
	}
	bundle := map[string]any{
		"resourceType": "Bundle",
		"id":           deterministicID("bundle", code, trigger, msgKey),
		"type":         "transaction",
		"timestamp":    msgTimestamp,
		"entry":        b.entries,
	}
	return &MapResult{
		Bundle:         bundle,
		MessageType:    msgTypeDisplay,
		TriggerEvent:   trigger,
		ControlID:      msg.ControlID(),
		PatientID:      patID,
		EncounterID:    encID,
		ResourceCounts: b.counts,
	}, nil
}

// ValFromMSH safely reads 1-based field & component from MSH.
func (m *Message) ValFromMSH(fieldSeq, compIdx int) string {
	msh := m.Segment("MSH")
	if msh == nil {
		return ""
	}
	return msh.Val(fieldSeq, compIdx)
}

type bundleBuilder struct {
	sysPrefix  string
	now        time.Time
	entries    []any
	seenIDs    map[string]int
	targetRefs []string
	counts     map[string]int
}

func (b *bundleBuilder) upsert(resType, id string, resMap map[string]any) {
	b.upsertInternal(resType, id, resMap, true)
}

func (b *bundleBuilder) upsertNoTarget(resType, id string, resMap map[string]any) {
	b.upsertInternal(resType, id, resMap, false)
}

func (b *bundleBuilder) upsertInternal(resType, id string, resMap map[string]any, trackTarget bool) {
	ref := resType + "/" + id
	entry := map[string]any{
		"fullUrl":  ref,
		"resource": resMap,
		"request": map[string]any{
			"method": "PUT",
			"url":    ref,
		},
	}
	if existingIdx, exists := b.seenIDs[ref]; exists {
		b.entries[existingIdx] = entry
		return
	}
	b.seenIDs[ref] = len(b.entries)
	b.entries = append(b.entries, entry)
	b.counts[resType]++
	if trackTarget {
		b.targetRefs = append(b.targetRefs, ref)
	}
}

func (b *bundleBuilder) mapPatient(pid *Segment, nk1s []*Segment, orgID string, fallbackKey string) (string, map[string]any) {
	var identifiers []any
	var primaryAssigner, primaryValue string

	// PID-3 Patient Identifier List (repeating CX)
	for _, rep := range pid.Field(3).Repetitions {
		idVal := strings.TrimSpace(rep.Comp(1))
		if idVal == "" {
			continue
		}
		assigner := firstNonEmptyStr(rep.Sub(4, 2), rep.Sub(4, 1))
		typeCode := strings.ToUpper(strings.TrimSpace(rep.Comp(5)))
		sysURI := normalizeAssigningAuthority(assigner, b.sysPrefix+"/mrn")
		if primaryValue == "" || (typeCode == "MR" && primaryAssigner == "") {
			primaryAssigner = sysURI
			primaryValue = idVal
		}
		idObj := map[string]any{
			"system": sysURI,
			"value":  idVal,
		}
		if typeCode != "" {
			idObj["type"] = map[string]any{
				"coding": []any{
					map[string]any{
						"system": "http://terminology.hl7.org/CodeSystem/v2-0203",
						"code":   typeCode,
					},
				},
			}
		}
		identifiers = append(identifiers, idObj)
	}

	// PID-2 fallback legacy patient ID if PID-3 was empty
	if len(identifiers) == 0 {
		if altID := strings.TrimSpace(pid.Val(2, 1)); altID != "" {
			sysURI := normalizeAssigningAuthority(pid.Val(2, 4), b.sysPrefix+"/patient-id")
			primaryAssigner = sysURI
			primaryValue = altID
			identifiers = append(identifiers, map[string]any{
				"system": sysURI,
				"value":  altID,
			})
		}
	}

	// PID-19 SSN Number
	if ssn := strings.TrimSpace(pid.Str(19)); ssn != "" {
		identifiers = append(identifiers, map[string]any{
			"system": "http://hl7.org/fhir/sid/us-ssn",
			"value":  ssn,
			"type": map[string]any{
				"coding": []any{
					map[string]any{
						"system": "http://terminology.hl7.org/CodeSystem/v2-0203",
						"code":   "SS",
					},
				},
			},
		})
	}

	// PID-5 Patient Name (repeating XPN)
	var names []any
	for _, rep := range pid.Field(5).Repetitions {
		if nm := mapXPNToHumanName(rep); nm != nil {
			names = append(names, nm)
		}
	}

	// BirthDate (PID-7)
	birthDate, hasBirth := ParseTimestamp(pid.Val(7, 1), true)

	// Deterministic Patient ID
	var patID string
	if primaryValue != "" {
		patID = deterministicID("pat", primaryAssigner, primaryValue)
	} else {
		patID = deterministicID("pat", pid.Val(5, 1), pid.Val(5, 2), birthDate, fallbackKey)
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
	if hasBirth {
		pat["birthDate"] = birthDate
	}
	if g := mapHL7Gender(pid.Str(8)); g != "" {
		pat["gender"] = g
	}

	// PID-11 Address (repeating XAD)
	var addrs []any
	for _, rep := range pid.Field(11).Repetitions {
		if addr := mapXADToAddress(rep); addr != nil {
			addrs = append(addrs, addr)
		}
	}
	if len(addrs) > 0 {
		pat["address"] = addrs
	}

	// PID-13 Home Phone & PID-14 Business Phone (repeating XTN)
	var telecoms []any
	for _, rep := range pid.Field(13).Repetitions {
		if tel := mapXTNToContactPoint(rep, "home"); tel != nil {
			telecoms = append(telecoms, tel)
		}
	}
	for _, rep := range pid.Field(14).Repetitions {
		if tel := mapXTNToContactPoint(rep, "work"); tel != nil {
			telecoms = append(telecoms, tel)
		}
	}
	if len(telecoms) > 0 {
		pat["telecom"] = telecoms
	}

	// PID-15 Primary Language
	if langCode := strings.TrimSpace(pid.Val(15, 1)); langCode != "" {
		langText := firstNonEmptyStr(pid.Val(15, 2), langCode)
		pat["communication"] = []any{
			map[string]any{
				"language": map[string]any{
					"coding": []any{
						map[string]any{
							"system":  "urn:ietf:bcp:47",
							"code":    normalizeLanguageBCP47(langCode),
							"display": langText,
						},
					},
					"text": langText,
				},
				"preferred": true,
			},
		}
	}

	// PID-16 Marital Status
	if ms := strings.ToUpper(strings.TrimSpace(pid.Val(16, 1))); ms != "" {
		if v3ms := mapMaritalStatus(ms); v3ms != "" {
			pat["maritalStatus"] = map[string]any{
				"coding": []any{
					map[string]any{
						"system": "http://terminology.hl7.org/CodeSystem/v3-MaritalStatus",
						"code":   v3ms,
					},
				},
			}
		}
	}

	// US Core Race (PID-10) & Ethnicity (PID-22) extensions
	var exts []any
	if raceExt := mapOMBRaceExtension(pid.Field(10)); raceExt != nil {
		exts = append(exts, raceExt)
	}
	if ethExt := mapOMBEthnicityExtension(pid.Field(22)); ethExt != nil {
		exts = append(exts, ethExt)
	}
	if len(exts) > 0 {
		pat["extension"] = exts
	}

	// PID-29 Death Date/Time & PID-30 Death Indicator
	if deathDT, ok := formatHL7DateTime(pid.Val(29, 1)); ok {
		pat["deceasedDateTime"] = deathDT
	} else if strings.EqualFold(strings.TrimSpace(pid.Str(30)), "Y") {
		pat["deceasedBoolean"] = true
	}

	if orgID != "" {
		pat["managingOrganization"] = map[string]any{"reference": "Organization/" + orgID}
	}

	// Inline Patient.contact from NK1
	var contacts []any
	for _, nk1 := range nk1s {
		if c := mapNK1ToPatientContact(nk1); c != nil {
			contacts = append(contacts, c)
		}
	}
	if len(contacts) > 0 {
		pat["contact"] = contacts
	}

	return patID, pat
}

func (b *bundleBuilder) mapRelatedPerson(nk1 *Segment, patID string, seq int) (string, map[string]any) {
	if nk1 == nil {
		return "", nil
	}
	var names []any
	for _, rep := range nk1.Field(2).Repetitions {
		if nm := mapXPNToHumanName(rep); nm != nil {
			names = append(names, nm)
		}
	}
	if len(names) == 0 {
		return "", nil
	}
	rpID := deterministicID("rel", patID, strconv.Itoa(seq), nk1.Val(2, 1), nk1.Val(2, 2))
	rp := map[string]any{
		"resourceType": "RelatedPerson",
		"id":           rpID,
		"active":       true,
		"patient":      map[string]any{"reference": "Patient/" + patID},
		"name":         names,
	}
	if relCode := strings.TrimSpace(nk1.Val(3, 1)); relCode != "" {
		rp["relationship"] = []any{
			mapCEField(nk1.Field(3), "http://terminology.hl7.org/CodeSystem/v2-0063"),
		}
	}
	var telecoms []any
	for _, rep := range nk1.Field(5).Repetitions {
		if tel := mapXTNToContactPoint(rep, "home"); tel != nil {
			telecoms = append(telecoms, tel)
		}
	}
	if len(telecoms) > 0 {
		rp["telecom"] = telecoms
	}
	var addrs []any
	for _, rep := range nk1.Field(4).Repetitions {
		if addr := mapXADToAddress(rep); addr != nil {
			addrs = append(addrs, addr)
		}
	}
	if len(addrs) > 0 {
		rp["address"] = addrs
	}
	return rpID, rp
}

func (b *bundleBuilder) mapVisit(pv1 *Segment, evn *Segment, msgCode, trigger, patID, orgID, fallbackKey string) string {
	patientClass := strings.ToUpper(strings.TrimSpace(pv1.Str(2)))
	// PV1-2 == "N" means Not Applicable (often sent in VXU/ORU without an active visit)
	if patientClass == "" || (patientClass == "N" && pv1.Val(19, 1) == "" && pv1.Val(44, 1) == "" && msgCode != "ADT") {
		return ""
	}

	visitNum := strings.TrimSpace(pv1.Val(19, 1))
	visitAuth := normalizeAssigningAuthority(firstNonEmptyStr(pv1.SubFromSeg(19, 4, 2), pv1.SubFromSeg(19, 4, 1)), b.sysPrefix+"/visit")
	admitTime, hasAdmit := formatHL7DateTime(pv1.Val(44, 1))
	dischargeTime, hasDischarge := formatHL7DateTime(pv1.Val(45, 1))
	if !hasAdmit && evn != nil {
		if evnDT, ok := formatHL7DateTime(evn.Val(2, 1)); ok && trigger != "A03" {
			admitTime, hasAdmit = evnDT, true
		}
	}
	if !hasDischarge && trigger == "A03" && evn != nil {
		if evnDT, ok := formatHL7DateTime(evn.Val(2, 1)); ok {
			dischargeTime, hasDischarge = evnDT, true
		}
	}

	var encID string
	if visitNum != "" {
		encID = deterministicID("enc", patID, visitAuth, visitNum)
	} else if hasAdmit {
		encID = deterministicID("enc", patID, admitTime)
	} else {
		encID = deterministicID("enc", patID, fallbackKey)
	}

	actClassCode, actClassDisplay := mapPatientClass(patientClass, trigger)
	status := "in-progress"
	switch trigger {
	case "A03":
		status = "finished"
	case "A05", "A14":
		status = "planned"
	case "A11", "A38":
		status = "cancelled"
	default:
		if hasDischarge {
			status = "finished"
		}
	}

	enc := map[string]any{
		"resourceType": "Encounter",
		"id":           encID,
		"status":       status,
		"class": map[string]any{
			"system":  "http://terminology.hl7.org/CodeSystem/v3-ActCode",
			"code":    actClassCode,
			"display": actClassDisplay,
		},
		"type": []any{
			map[string]any{
				"coding": []any{
					map[string]any{
						"system":  "http://snomed.info/sct",
						"code":    "185349003",
						"display": "Encounter for check up",
					},
				},
				"text": actClassDisplay,
			},
		},
		"subject": map[string]any{"reference": "Patient/" + patID},
	}

	if visitNum != "" {
		enc["identifier"] = []any{
			map[string]any{
				"system": visitAuth,
				"value":  visitNum,
				"type": map[string]any{
					"coding": []any{
						map[string]any{
							"system": "http://terminology.hl7.org/CodeSystem/v2-0203",
							"code":   "VN",
						},
					},
				},
			},
		}
	}

	if hasAdmit || hasDischarge {
		period := map[string]any{}
		if hasAdmit {
			period["start"] = admitTime
		}
		if hasDischarge {
			period["end"] = dischargeTime
		}
		enc["period"] = period
	}

	// PV1-3 Assigned Patient Location (PL: PointOfCare^Room^Bed^Facility)
	if poc := strings.TrimSpace(pv1.Val(3, 1)); poc != "" {
		room := strings.TrimSpace(pv1.Val(3, 2))
		bed := strings.TrimSpace(pv1.Val(3, 3))
		fac := strings.TrimSpace(pv1.Val(3, 4))
		locParts := []string{poc}
		if room != "" {
			locParts = append(locParts, "Room "+room)
		}
		if bed != "" {
			locParts = append(locParts, "Bed "+bed)
		}
		locName := strings.Join(locParts, " - ")
		locID := deterministicID("loc", fac, poc, room, bed)
		locRes := map[string]any{
			"resourceType": "Location",
			"id":           locID,
			"status":       "active",
			"name":         locName,
			"mode":         "instance",
		}
		if orgID != "" {
			locRes["managingOrganization"] = map[string]any{"reference": "Organization/" + orgID}
		}
		b.upsert("Location", locID, locRes)
		enc["location"] = []any{
			map[string]any{
				"location": map[string]any{
					"reference": "Location/" + locID,
					"display":   locName,
				},
			},
		}
	}

	// PV1-7 Attending, PV1-8 Referring, PV1-9 Consulting, PV1-17 Admitting Doctor (XCN)
	type docRole struct {
		field int
		code  string
	}
	var participants []any
	for _, dr := range []docRole{{7, "ATND"}, {8, "REF"}, {9, "CON"}, {17, "ADM"}} {
		for _, rep := range pv1.Field(dr.field).Repetitions {
			pracID, pracMap := b.mapXCNPractitioner(rep)
			if pracMap == nil {
				continue
			}
			b.upsert("Practitioner", pracID, pracMap)
			participants = append(participants, map[string]any{
				"type": []any{
					map[string]any{
						"coding": []any{
							map[string]any{
								"system": "http://terminology.hl7.org/CodeSystem/v3-ParticipationType",
								"code":   dr.code,
							},
						},
					},
				},
				"individual": map[string]any{"reference": "Practitioner/" + pracID},
			})
		}
	}
	if len(participants) > 0 {
		enc["participant"] = participants
	}
	if orgID != "" {
		enc["serviceProvider"] = map[string]any{"reference": "Organization/" + orgID}
	}

	b.upsert("Encounter", encID, enc)
	return encID
}

func (b *bundleBuilder) mapSyntheticADTEncounter(evn *Segment, trigger, patID, orgID, fallbackKey string) string {
	encID := deterministicID("enc", patID, fallbackKey)
	status := "in-progress"
	if trigger == "A03" {
		status = "finished"
	}
	actCode, actDisplay := mapPatientClass("", trigger)
	enc := map[string]any{
		"resourceType": "Encounter",
		"id":           encID,
		"status":       status,
		"class": map[string]any{
			"system":  "http://terminology.hl7.org/CodeSystem/v3-ActCode",
			"code":    actCode,
			"display": actDisplay,
		},
		"type": []any{
			map[string]any{
				"coding": []any{
					map[string]any{
						"system":  "http://snomed.info/sct",
						"code":    "185349003",
						"display": "Encounter for check up",
					},
				},
			},
		},
		"subject": map[string]any{"reference": "Patient/" + patID},
	}
	if evn != nil {
		if dt, ok := formatHL7DateTime(evn.Val(2, 1)); ok {
			if trigger == "A03" {
				enc["period"] = map[string]any{"end": dt}
			} else {
				enc["period"] = map[string]any{"start": dt}
			}
		}
	}
	if orgID != "" {
		enc["serviceProvider"] = map[string]any{"reference": "Organization/" + orgID}
	}
	b.upsert("Encounter", encID, enc)
	return encID
}

func (b *bundleBuilder) mapAllergy(al1 *Segment, patID string, seq int) (string, map[string]any) {
	if al1 == nil || al1.Field(3).IsEmpty() {
		return "", nil
	}
	codeCC := mapCEField(al1.Field(3), "http://www.nlm.nih.gov/research/umls/rxnorm")
	codeKey := firstNonEmptyStr(al1.Val(3, 1), al1.Val(3, 2), strconv.Itoa(seq))
	alID := deterministicID("allergy", patID, codeKey)

	al := map[string]any{
		"resourceType": "AllergyIntolerance",
		"id":           alID,
		"clinicalStatus": map[string]any{
			"coding": []any{
				map[string]any{
					"system": "http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical",
					"code":   "active",
				},
			},
		},
		"verificationStatus": map[string]any{
			"coding": []any{
				map[string]any{
					"system": "http://terminology.hl7.org/CodeSystem/allergyintolerance-verification",
					"code":   "confirmed",
				},
			},
		},
		"code":    codeCC,
		"patient": map[string]any{"reference": "Patient/" + patID},
	}

	switch strings.ToUpper(strings.TrimSpace(al1.Val(2, 1))) {
	case "DA":
		al["category"] = []any{"medication"}
	case "FA":
		al["category"] = []any{"food"}
	case "EA", "MA", "PA", "LA":
		al["category"] = []any{"environment"}
	}

	sevCode := strings.ToUpper(strings.TrimSpace(al1.Val(4, 1)))
	var fhirSeverity string
	switch sevCode {
	case "SV":
		al["criticality"] = "high"
		fhirSeverity = "severe"
	case "MO":
		al["criticality"] = "low"
		fhirSeverity = "moderate"
	case "MI":
		al["criticality"] = "low"
		fhirSeverity = "mild"
	}

	var manifestations []any
	for _, rep := range al1.Field(5).Repetitions {
		txt := strings.TrimSpace(rep.Value())
		if txt != "" {
			manifestations = append(manifestations, map[string]any{"text": txt})
		}
	}
	if len(manifestations) > 0 {
		rxn := map[string]any{"manifestation": manifestations}
		if fhirSeverity != "" {
			rxn["severity"] = fhirSeverity
		}
		al["reaction"] = []any{rxn}
	}
	if onset, ok := formatHL7DateTime(al1.Val(6, 1)); ok {
		al["onsetDateTime"] = onset
	}
	return alID, al
}

func (b *bundleBuilder) mapCondition(dg1 *Segment, patID, encID string, seq int) (string, map[string]any) {
	if dg1 == nil {
		return "", nil
	}
	var codeCC map[string]any
	var codeKey string
	if !dg1.Field(3).IsEmpty() {
		codeCC = mapCEField(dg1.Field(3), "http://hl7.org/fhir/sid/icd-10-cm")
		codeKey = firstNonEmptyStr(dg1.Val(3, 1), dg1.Val(3, 2))
	} else if desc := strings.TrimSpace(dg1.Str(4)); desc != "" {
		codeCC = map[string]any{"text": desc}
		codeKey = desc
	} else {
		return "", nil
	}

	cID := deterministicID("cond", patID, encID, codeKey, strconv.Itoa(seq))
	cond := map[string]any{
		"resourceType": "Condition",
		"id":           cID,
		"clinicalStatus": map[string]any{
			"coding": []any{
				map[string]any{
					"system": "http://terminology.hl7.org/CodeSystem/condition-clinical",
					"code":   "active",
				},
			},
		},
		"verificationStatus": map[string]any{
			"coding": []any{
				map[string]any{
					"system": "http://terminology.hl7.org/CodeSystem/condition-ver-status",
					"code":   "confirmed",
				},
			},
		},
		"category": []any{
			map[string]any{
				"coding": []any{
					map[string]any{
						"system":  "http://terminology.hl7.org/CodeSystem/condition-category",
						"code":    "encounter-diagnosis",
						"display": "Encounter Diagnosis",
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
	if onset, ok := formatHL7DateTime(dg1.Val(5, 1)); ok {
		cond["onsetDateTime"] = onset
	}
	return cID, cond
}

func (b *bundleBuilder) mapORUGroups(msg *Message, patID, encID, msgKey, msgInstant string) {
	type obrGroup struct {
		orc     *Segment
		obr     *Segment
		obrNTEs []*Segment
		obxList []obxBundleItem
		spmList []*Segment
	}
	var groups []*obrGroup
	var cur *obrGroup

	for _, seg := range msg.Segments {
		switch seg.Name {
		case "ORC":
			if cur != nil && cur.obr != nil {
				groups = append(groups, cur)
				cur = nil
			}
			if cur == nil {
				cur = &obrGroup{}
			}
			cur.orc = seg
		case "OBR":
			if cur != nil && cur.obr != nil {
				groups = append(groups, cur)
				cur = nil
			}
			if cur == nil {
				cur = &obrGroup{}
			}
			cur.obr = seg
		case "OBX":
			if cur == nil {
				cur = &obrGroup{}
			}
			cur.obxList = append(cur.obxList, obxBundleItem{obx: seg})
		case "NTE":
			if cur != nil {
				if len(cur.obxList) > 0 {
					last := &cur.obxList[len(cur.obxList)-1]
					last.ntes = append(last.ntes, seg)
				} else {
					cur.obrNTEs = append(cur.obrNTEs, seg)
				}
			}
		case "SPM":
			if cur == nil {
				cur = &obrGroup{}
			}
			cur.spmList = append(cur.spmList, seg)
		}
	}
	if cur != nil {
		groups = append(groups, cur)
	}

	for gIdx, g := range groups {
		orderKey := extractOrderKey(g.orc, g.obr, msgKey, gIdx+1)
		var pracRef string
		if g.obr != nil && !g.obr.Field(16).IsEmpty() {
			if pID, pMap := b.mapXCNPractitioner(g.obr.Field(16).Repetitions[0]); pMap != nil {
				b.upsert("Practitioner", pID, pMap)
				pracRef = "Practitioner/" + pID
			}
		} else if g.orc != nil && !g.orc.Field(12).IsEmpty() {
			if pID, pMap := b.mapXCNPractitioner(g.orc.Field(12).Repetitions[0]); pMap != nil {
				b.upsert("Practitioner", pID, pMap)
				pracRef = "Practitioner/" + pID
			}
		}

		var srID string
		if g.obr != nil && (g.obr.Val(2, 1) != "" || g.obr.Val(3, 1) != "" || (g.orc != nil && (g.orc.Val(2, 1) != "" || g.orc.Val(3, 1) != ""))) {
			srID = deterministicID("sr", patID, orderKey)
			sr := map[string]any{
				"resourceType": "ServiceRequest",
				"id":           srID,
				"status":       "completed",
				"intent":       "order",
				"code":         mapCEFieldWithDefault(g.obr.Field(4), "http://loinc.org", "Laboratory panel"),
				"subject":      map[string]any{"reference": "Patient/" + patID},
				"identifier":   buildOrderIdentifiers(g.orc, g.obr, b.sysPrefix),
			}
			if encID != "" {
				sr["encounter"] = map[string]any{"reference": "Encounter/" + encID}
			}
			if pracRef != "" {
				sr["requester"] = map[string]any{"reference": pracRef}
			}
			b.upsert("ServiceRequest", srID, sr)
		}

		// Specimen(s) from SPM segments or OBR-15
		var specRefs []any
		for sIdx, spm := range g.spmList {
			spID := deterministicID("spec", patID, orderKey, spm.Val(2, 1), strconv.Itoa(sIdx+1))
			sp := map[string]any{
				"resourceType": "Specimen",
				"id":           spID,
				"status":       "available",
				"subject":      map[string]any{"reference": "Patient/" + patID},
			}
			if !spm.Field(4).IsEmpty() {
				sp["type"] = mapCEField(spm.Field(4), "http://snomed.info/sct")
			}
			if spm.Val(2, 1) != "" {
				sp["identifier"] = []any{
					map[string]any{
						"system": b.sysPrefix + "/specimen",
						"value":  spm.Val(2, 1),
					},
				}
			}
			if recDT, ok := formatHL7DateTime(spm.Val(18, 1)); ok {
				sp["receivedTime"] = recDT
			}
			if collDT, ok := formatHL7DateTime(spm.Val(17, 1)); ok {
				sp["collection"] = map[string]any{"collectedDateTime": collDT}
			}
			b.upsert("Specimen", spID, sp)
			specRefs = append(specRefs, map[string]any{"reference": "Specimen/" + spID})
		}

		var obrEffDT string
		if g.obr != nil {
			if dt, ok := formatHL7DateTime(g.obr.Val(7, 1)); ok {
				obrEffDT = dt
			}
		}

		var obsRefs []any
		for oIdx, item := range g.obxList {
			obsID, obsMap := b.mapObservation(item.obx, item.ntes, patID, encID, pracRef, orderKey, oIdx+1, obrEffDT, "laboratory")
			if obsMap != nil {
				b.upsert("Observation", obsID, obsMap)
				obsRefs = append(obsRefs, map[string]any{"reference": "Observation/" + obsID})
			}
		}

		if g.obr != nil || len(obsRefs) > 0 {
			drID := deterministicID("dr", patID, orderKey)
			drStatus := "final"
			var drCode map[string]any
			if g.obr != nil {
				drStatus = mapOBRStatus(g.obr.Str(25))
				drCode = mapCEFieldWithDefault(g.obr.Field(4), "http://loinc.org", "Diagnostic report")
			} else {
				drCode = map[string]any{
					"coding": []any{
						map[string]any{
							"system":  "http://loinc.org",
							"code":    "11502-2",
							"display": "Laboratory report",
						},
					},
				}
			}
			issued := msgInstant
			if g.obr != nil {
				if obrIssued, ok := formatHL7Instant(g.obr.Val(22, 1)); ok {
					issued = obrIssued
				}
			}
			dr := map[string]any{
				"resourceType": "DiagnosticReport",
				"id":           drID,
				"status":       drStatus,
				"category": []any{
					map[string]any{
						"coding": []any{
							map[string]any{
								"system":  "http://terminology.hl7.org/CodeSystem/v2-0074",
								"code":    "LAB",
								"display": "Laboratory",
							},
						},
					},
				},
				"code":    drCode,
				"subject": map[string]any{"reference": "Patient/" + patID},
				"issued":  issued,
			}
			if encID != "" {
				dr["encounter"] = map[string]any{"reference": "Encounter/" + encID}
			}
			if obrEffDT != "" {
				dr["effectiveDateTime"] = obrEffDT
			}
			if pracRef != "" {
				dr["performer"] = []any{map[string]any{"reference": pracRef}}
			}
			if srID != "" {
				dr["basedOn"] = []any{map[string]any{"reference": "ServiceRequest/" + srID}}
			}
			if len(specRefs) > 0 {
				dr["specimen"] = specRefs
			}
			if len(obsRefs) > 0 {
				dr["result"] = obsRefs
			}
			if notes := joinNTEText(g.obrNTEs); notes != "" {
				dr["conclusion"] = notes
			}
			if g.obr != nil {
				if ids := buildOrderIdentifiers(g.orc, g.obr, b.sysPrefix); len(ids) > 0 {
					dr["identifier"] = ids
				}
			}
			b.upsert("DiagnosticReport", drID, dr)
		}
	}
}

type obxBundleItem struct {
	obx  *Segment
	ntes []*Segment
}

func (b *bundleBuilder) mapObservation(
	obx *Segment,
	ntes []*Segment,
	patID, encID, pracRef, groupKey string,
	seq int,
	fallbackEffectiveDT string,
	categoryCode string,
) (string, map[string]any) {
	if obx == nil || obx.Field(3).IsEmpty() {
		return "", nil
	}
	subID := strings.TrimSpace(obx.Str(4))
	setID := firstNonEmptyStr(strings.TrimSpace(obx.Str(1)), strconv.Itoa(seq))
	codeKey := firstNonEmptyStr(obx.Val(3, 1), obx.Val(3, 2))
	obsID := deterministicID("obs", patID, groupKey, setID, subID, codeKey)

	status := mapOBXStatus(obx.Str(11))
	obs := map[string]any{
		"resourceType": "Observation",
		"id":           obsID,
		"status":       status,
		"category": []any{
			map[string]any{
				"coding": []any{
					map[string]any{
						"system": "http://terminology.hl7.org/CodeSystem/observation-category",
						"code":   categoryCode,
					},
				},
			},
		},
		"code":    mapCEFieldWithDefault(obx.Field(3), "http://loinc.org", "Observation"),
		"subject": map[string]any{"reference": "Patient/" + patID},
	}
	if encID != "" {
		obs["encounter"] = map[string]any{"reference": "Encounter/" + encID}
	}
	if pracRef != "" {
		obs["performer"] = []any{map[string]any{"reference": pracRef}}
	}
	if effDT, ok := formatHL7DateTime(obx.Val(14, 1)); ok {
		obs["effectiveDateTime"] = effDT
	} else if fallbackEffectiveDT != "" {
		obs["effectiveDateTime"] = fallbackEffectiveDT
	}

	valType := strings.ToUpper(strings.TrimSpace(obx.Str(2)))
	applyOBXValue(obs, valType, obx)

	// OBX-7 Reference Range
	if refRange := strings.TrimSpace(obx.Str(7)); refRange != "" {
		obs["referenceRange"] = []any{parseReferenceRange(refRange, obx.Field(6))}
	}

	// OBX-8 Abnormal Flags
	var interps []any
	for _, rep := range obx.Field(8).Repetitions {
		flag := strings.ToUpper(strings.TrimSpace(rep.Value()))
		if flag == "" {
			continue
		}
		interps = append(interps, map[string]any{
			"coding": []any{
				map[string]any{
					"system": "http://terminology.hl7.org/CodeSystem/v3-ObservationInterpretation",
					"code":   flag,
				},
			},
		})
	}
	if len(interps) > 0 {
		obs["interpretation"] = interps
	}

	// Child NTE comments
	var noteArr []any
	for _, nte := range ntes {
		if txt := extractSingleNTE(nte); txt != "" {
			noteArr = append(noteArr, map[string]any{"text": txt})
		}
	}
	if len(noteArr) > 0 {
		obs["note"] = noteArr
	}

	return obsID, obs
}

func applyOBXValue(obs map[string]any, valType string, obx *Segment) {
	if obx.Field(5).IsEmpty() {
		return
	}
	unitCode := strings.TrimSpace(obx.Val(6, 1))
	unitText := firstNonEmptyStr(strings.TrimSpace(obx.Val(6, 2)), unitCode)

	switch valType {
	case "NM":
		rawNum := strings.TrimSpace(obx.Val(5, 1))
		comp, numClean := splitLeadingComparator(rawNum)
		if f, err := strconv.ParseFloat(numClean, 64); err == nil {
			q := map[string]any{"value": f}
			if comp != "" {
				q["comparator"] = comp
			}
			applyUCUMUnits(q, unitCode, unitText)
			obs["valueQuantity"] = q
		} else if rawNum != "" {
			obs["valueString"] = rawNum
		}
	case "SN":
		// Structured Numeric: <comparator> ^ <num1> ^ <separator/suffix> ^ <num2>
		c1 := strings.TrimSpace(obx.Val(5, 1))
		n1Str := strings.TrimSpace(obx.Val(5, 2))
		sep := strings.TrimSpace(obx.Val(5, 3))
		n2Str := strings.TrimSpace(obx.Val(5, 4))
		n1, err1 := strconv.ParseFloat(n1Str, 64)
		n2, err2 := strconv.ParseFloat(n2Str, 64)
		switch {
		case sep == "-" && err1 == nil && err2 == nil:
			lowQ := map[string]any{"value": n1}
			highQ := map[string]any{"value": n2}
			applyUCUMUnits(lowQ, unitCode, unitText)
			applyUCUMUnits(highQ, unitCode, unitText)
			obs["valueRange"] = map[string]any{"low": lowQ, "high": highQ}
		case (sep == ":" || sep == "/") && err1 == nil && err2 == nil:
			obs["valueRatio"] = map[string]any{
				"numerator":   map[string]any{"value": n1},
				"denominator": map[string]any{"value": n2},
			}
		case err1 == nil:
			q := map[string]any{"value": n1}
			if c1 == "<" || c1 == "<=" || c1 == ">" || c1 == ">=" {
				q["comparator"] = c1
			}
			applyUCUMUnits(q, unitCode, unitText)
			obs["valueQuantity"] = q
		default:
			obs["valueString"] = obx.Field(5).Repetitions[0].FormatJoined(DefaultDelimiters())
		}
	case "CE", "CWE", "CF":
		obs["valueCodeableConcept"] = mapCEField(obx.Field(5), "http://snomed.info/sct")
	case "DT":
		if d, ok := ParseTimestamp(obx.Val(5, 1), true); ok {
			obs["valueDateTime"] = d
		} else {
			obs["valueString"] = strings.TrimSpace(obx.Val(5, 1))
		}
	case "TS", "DTM":
		if dt, ok := formatHL7DateTime(obx.Val(5, 1)); ok {
			obs["valueDateTime"] = dt
		} else {
			obs["valueString"] = strings.TrimSpace(obx.Val(5, 1))
		}
	case "TM":
		if tm := formatHL7Time(obx.Val(5, 1)); tm != "" {
			obs["valueTime"] = tm
		} else {
			obs["valueString"] = strings.TrimSpace(obx.Val(5, 1))
		}
	default:
		// ST, TX, FT, ED, etc.
		var parts []string
		for _, rep := range obx.Field(5).Repetitions {
			if v := strings.TrimSpace(rep.Value()); v != "" {
				parts = append(parts, v)
			}
		}
		if len(parts) > 0 {
			obs["valueString"] = strings.Join(parts, "\n")
		}
	}
}

func (b *bundleBuilder) mapVXUGroups(msg *Message, patID, encID, msgKey string) {
	type rxaGroup struct {
		orc  *Segment
		rxa  *Segment
		rxr  *Segment
		obxs []obxBundleItem
	}
	var groups []*rxaGroup
	var cur *rxaGroup

	for _, seg := range msg.Segments {
		switch seg.Name {
		case "ORC":
			if cur != nil && cur.rxa != nil {
				groups = append(groups, cur)
				cur = nil
			}
			if cur == nil {
				cur = &rxaGroup{}
			}
			cur.orc = seg
		case "RXA":
			if cur != nil && cur.rxa != nil {
				groups = append(groups, cur)
				cur = nil
			}
			if cur == nil {
				cur = &rxaGroup{}
			}
			cur.rxa = seg
		case "RXR":
			if cur != nil {
				cur.rxr = seg
			}
		case "OBX":
			if cur != nil {
				cur.obxs = append(cur.obxs, obxBundleItem{obx: seg})
			}
		case "NTE":
			if cur != nil && len(cur.obxs) > 0 {
				last := &cur.obxs[len(cur.obxs)-1]
				last.ntes = append(last.ntes, seg)
			}
		}
	}
	if cur != nil && cur.rxa != nil {
		groups = append(groups, cur)
	}

	for gIdx, g := range groups {
		rxa := g.rxa
		cvxCode := strings.TrimSpace(rxa.Val(5, 1))
		occDT, hasOcc := formatHL7DateTime(rxa.Val(3, 1))
		if !hasOcc {
			occDT = b.now.Format("2006-01-02")
		}
		orderKey := extractOrderKey(g.orc, nil, "", 0)
		var immID string
		if orderKey != "" {
			immID = deterministicID("imm", patID, orderKey, cvxCode)
		} else {
			immID = deterministicID("imm", patID, cvxCode, occDT, strconv.Itoa(gIdx+1), msgKey)
		}

		compStatus := strings.ToUpper(strings.TrimSpace(rxa.Str(20)))
		immStatus := "completed"
		if compStatus == "RE" || compStatus == "NA" || cvxCode == "998" {
			immStatus = "not-done"
		} else if compStatus == "PA" {
			immStatus = "completed"
		}

		// RXA-9 Administration Notes (00 = New immunization record -> primarySource=true)
		primarySource := true
		if infoSrc := strings.TrimSpace(rxa.Val(9, 1)); infoSrc != "" && infoSrc != "00" {
			primarySource = false
		}

		imm := map[string]any{
			"resourceType":       "Immunization",
			"id":                 immID,
			"status":             immStatus,
			"vaccineCode":        mapCEFieldWithDefault(rxa.Field(5), "http://hl7.org/fhir/sid/cvx", "Vaccine"),
			"patient":            map[string]any{"reference": "Patient/" + patID},
			"occurrenceDateTime": occDT,
			"primarySource":      primarySource,
		}
		if encID != "" {
			imm["encounter"] = map[string]any{"reference": "Encounter/" + encID}
		}
		if !primarySource && !rxa.Field(9).IsEmpty() {
			imm["reportOrigin"] = mapCEField(rxa.Field(9), "http://terminology.hl7.org/CodeSystem/immunization-origin")
		}
		if !rxa.Field(18).IsEmpty() && immStatus == "not-done" {
			imm["statusReason"] = mapCEField(rxa.Field(18), "http://terminology.hl7.org/CodeSystem/v3-ActReason")
		}

		// RXA-6 Administered Amount & RXA-7 Units ("999" in NIST IZ messages means unknown amount)
		if amtStr := strings.TrimSpace(rxa.Str(6)); amtStr != "" && amtStr != "999" {
			if amt, err := strconv.ParseFloat(amtStr, 64); err == nil {
				dq := map[string]any{"value": amt}
				applyUCUMUnits(dq, strings.TrimSpace(rxa.Val(7, 1)), strings.TrimSpace(rxa.Val(7, 2)))
				imm["doseQuantity"] = dq
			}
		}

		// RXA-15 Lot Number & RXA-16 Expiration Date
		if lot := strings.TrimSpace(rxa.Val(15, 1)); lot != "" {
			imm["lotNumber"] = lot
		}
		if expDate, ok := ParseTimestamp(rxa.Val(16, 1), true); ok && len(expDate) == 10 {
			imm["expirationDate"] = expDate
		}

		// RXA-17 Substance Manufacturer Name (MVX)
		if mfgCode := strings.TrimSpace(rxa.Val(17, 1)); mfgCode != "" || strings.TrimSpace(rxa.Val(17, 2)) != "" {
			mfgName := firstNonEmptyStr(strings.TrimSpace(rxa.Val(17, 2)), mfgCode)
			mfgID := deterministicID("org-mvx", mfgCode, mfgName)
			mfgOrg := map[string]any{
				"resourceType": "Organization",
				"id":           mfgID,
				"active":       true,
				"name":         mfgName,
			}
			if mfgCode != "" {
				mfgOrg["identifier"] = []any{
					map[string]any{
						"system": "http://hl7.org/fhir/sid/mvx",
						"value":  mfgCode,
					},
				}
			}
			b.upsert("Organization", mfgID, mfgOrg)
			imm["manufacturer"] = map[string]any{
				"reference": "Organization/" + mfgID,
				"display":   mfgName,
			}
		}

		// RXR Route & Administration Site
		if g.rxr != nil {
			if !g.rxr.Field(1).IsEmpty() {
				imm["route"] = mapCEField(g.rxr.Field(1), "http://terminology.hl7.org/CodeSystem/v2-0162")
			}
			if !g.rxr.Field(2).IsEmpty() {
				imm["site"] = mapCEField(g.rxr.Field(2), "http://terminology.hl7.org/CodeSystem/v2-0163")
			}
		}

		if g.orc != nil {
			if ids := buildOrderIdentifiers(g.orc, nil, b.sysPrefix); len(ids) > 0 {
				imm["identifier"] = ids
			}
		}

		b.upsert("Immunization", immID, imm)

		// Child OBX observations (e.g. VFC eligibility 64994-7, VIS dates)
		for oIdx, item := range g.obxs {
			obsID, obsMap := b.mapObservation(item.obx, item.ntes, patID, encID, "", immID, oIdx+1, occDT, "exam")
			if obsMap != nil {
				obsMap["partOf"] = []any{map[string]any{"reference": "Immunization/" + immID}}
				b.upsert("Observation", obsID, obsMap)
			}
		}
	}
}

func (b *bundleBuilder) mapORMOrders(msg *Message, patID, encID string, condRefs []any, msgKey, msgInstant string) {
	type ormGroup struct {
		orc  *Segment
		obr  *Segment
		ntes []*Segment
	}
	var groups []*ormGroup
	var cur *ormGroup

	for _, seg := range msg.Segments {
		switch seg.Name {
		case "ORC":
			if cur != nil && (cur.orc != nil || cur.obr != nil) {
				groups = append(groups, cur)
				cur = nil
			}
			cur = &ormGroup{orc: seg}
		case "OBR":
			if cur != nil && cur.obr != nil {
				groups = append(groups, cur)
				cur = nil
			}
			if cur == nil {
				cur = &ormGroup{}
			}
			cur.obr = seg
		case "NTE":
			if cur != nil {
				cur.ntes = append(cur.ntes, seg)
			}
		}
	}
	if cur != nil && (cur.orc != nil || cur.obr != nil) {
		groups = append(groups, cur)
	}

	for gIdx, g := range groups {
		orderKey := extractOrderKey(g.orc, g.obr, msgKey, gIdx+1)
		srID := deterministicID("sr", patID, orderKey)

		orderCtrl := ""
		orderStatus := ""
		if g.orc != nil {
			orderCtrl = strings.ToUpper(strings.TrimSpace(g.orc.Str(1)))
			orderStatus = strings.ToUpper(strings.TrimSpace(g.orc.Str(5)))
		}
		srStatus := mapORMOrderStatus(orderCtrl, orderStatus)

		var codeCC map[string]any
		if g.obr != nil && !g.obr.Field(4).IsEmpty() {
			codeCC = mapCEField(g.obr.Field(4), "http://loinc.org")
		} else {
			codeCC = map[string]any{
				"coding": []any{
					map[string]any{
						"system":  "http://snomed.info/sct",
						"code":    "108252007",
						"display": "Laboratory procedure",
					},
				},
			}
		}

		sr := map[string]any{
			"resourceType": "ServiceRequest",
			"id":           srID,
			"status":       srStatus,
			"intent":       "order",
			"code":         codeCC,
			"subject":      map[string]any{"reference": "Patient/" + patID},
		}
		if encID != "" {
			sr["encounter"] = map[string]any{"reference": "Encounter/" + encID}
		}
		if ids := buildOrderIdentifiers(g.orc, g.obr, b.sysPrefix); len(ids) > 0 {
			sr["identifier"] = ids
		}

		// Priority from OBR-27.6, OBR-5, or ORC-7.6
		var prioRaw string
		if g.obr != nil {
			prioRaw = firstNonEmptyStr(g.obr.Val(27, 6), g.obr.Str(5))
		}
		if prioRaw == "" && g.orc != nil {
			prioRaw = g.orc.Val(7, 6)
		}
		if prio := mapOrderPriority(prioRaw); prio != "" {
			sr["priority"] = prio
		}

		// AuthoredOn from ORC-9 or MSH-7
		if g.orc != nil {
			if dt, ok := formatHL7DateTime(g.orc.Val(9, 1)); ok {
				sr["authoredOn"] = dt
			}
		}
		if _, hasAuth := sr["authoredOn"]; !hasAuth && msgInstant != "" {
			sr["authoredOn"] = msgInstant
		}

		// Ordering Provider (ORC-12 or OBR-16)
		var pracRep *Repetition
		if g.orc != nil && len(g.orc.Field(12).Repetitions) > 0 {
			pracRep = &g.orc.Field(12).Repetitions[0]
		} else if g.obr != nil && len(g.obr.Field(16).Repetitions) > 0 {
			pracRep = &g.obr.Field(16).Repetitions[0]
		}
		if pracRep != nil {
			if pID, pMap := b.mapXCNPractitioner(*pracRep); pMap != nil {
				b.upsert("Practitioner", pID, pMap)
				sr["requester"] = map[string]any{"reference": "Practitioner/" + pID}
			}
		}

		if len(condRefs) > 0 {
			sr["reasonReference"] = condRefs
		}
		var notes []any
		for _, nte := range g.ntes {
			if txt := extractSingleNTE(nte); txt != "" {
				notes = append(notes, map[string]any{"text": txt})
			}
		}
		if len(notes) > 0 {
			sr["note"] = notes
		}

		b.upsert("ServiceRequest", srID, sr)
	}
}

func (b *bundleBuilder) mapXCNPractitioner(rep Repetition) (string, map[string]any) {
	idVal := strings.TrimSpace(rep.Comp(1))
	family := strings.TrimSpace(rep.Comp(2))
	given := strings.TrimSpace(rep.Comp(3))
	middle := strings.TrimSpace(rep.Comp(4))
	suffix := strings.TrimSpace(rep.Comp(5))
	prefix := strings.TrimSpace(rep.Comp(6))
	if idVal == "" && family == "" && given == "" {
		return "", nil
	}
	assigner := firstNonEmptyStr(rep.Sub(9, 2), rep.Sub(9, 1))
	idSys := normalizeAssigningAuthority(assigner, "http://hl7.org/fhir/sid/us-npi")
	pracID := deterministicID("prac", idSys, idVal, family, given)

	prac := map[string]any{
		"resourceType": "Practitioner",
		"id":           pracID,
		"active":       true,
	}
	if idVal != "" {
		prac["identifier"] = []any{
			map[string]any{
				"system": idSys,
				"value":  idVal,
			},
		}
	}
	if family != "" || given != "" {
		nm := map[string]any{}
		if family != "" {
			nm["family"] = family
		}
		var givens []any
		if given != "" {
			givens = append(givens, given)
		}
		if middle != "" {
			givens = append(givens, middle)
		}
		if len(givens) > 0 {
			nm["given"] = givens
		}
		if prefix != "" {
			nm["prefix"] = []any{prefix}
		}
		if suffix != "" {
			nm["suffix"] = []any{suffix}
		}
		prac["name"] = []any{nm}
	}
	return pracID, prac
}

// SubFromSeg reads 1-based (field, comp, sub) on the first repetition of seg.
func (s *Segment) SubFromSeg(fieldSeq, compIdx, subIdx int) string {
	return s.Get(fieldSeq, 1, compIdx, subIdx)
}

func deterministicID(prefix string, parts ...string) string {
	token := deterministicToken(prefix, parts...)
	return "hl7v2-" + prefix + "-" + token
}

func deterministicToken(prefix string, parts ...string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(prefix))
	for _, p := range parts {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(strings.TrimSpace(p)))
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:10])
}

func normalizeAssigningAuthority(raw string, defaultURI string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return defaultURI
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "urn:") {
		return s
	}
	if isOID(s) {
		switch s {
		case "2.16.840.1.113883.4.6":
			return "http://hl7.org/fhir/sid/us-npi"
		case "2.16.840.1.113883.4.1":
			return "http://hl7.org/fhir/sid/us-ssn"
		default:
			return "urn:oid:" + s
		}
	}
	upper := strings.ToUpper(s)
	if upper == "NPI" || upper == "USNPI" {
		return "http://hl7.org/fhir/sid/us-npi"
	}
	if upper == "SSN" || upper == "USSSA" {
		return "http://hl7.org/fhir/sid/us-ssn"
	}
	slug := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return '-'
	}, s)
	slug = strings.Trim(slug, "-")
	if slug == "" {
		return defaultURI
	}
	return "urn:flint:hl7v2:assigner:" + slug
}

func mapCodeSystemURI(hl7Sys string, defaultSystem string) string {
	s := strings.TrimSpace(hl7Sys)
	if s == "" {
		return defaultSystem
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "urn:") {
		return s
	}
	if isOID(s) {
		switch s {
		case "2.16.840.1.113883.6.1":
			return "http://loinc.org"
		case "2.16.840.1.113883.6.96":
			return "http://snomed.info/sct"
		case "2.16.840.1.113883.6.88":
			return "http://www.nlm.nih.gov/research/umls/rxnorm"
		case "2.16.840.1.113883.12.292":
			return "http://hl7.org/fhir/sid/cvx"
		case "2.16.840.1.113883.6.90":
			return "http://hl7.org/fhir/sid/icd-10-cm"
		case "2.16.840.1.113883.6.69":
			return "http://hl7.org/fhir/sid/ndc"
		case "2.16.840.1.113883.6.12":
			return "http://www.ama-assn.org/go/cpt"
		case "2.16.840.1.113883.6.8":
			return "http://unitsofmeasure.org"
		default:
			return "urn:oid:" + s
		}
	}
	switch strings.ToUpper(s) {
	case "LN", "LOINC", "LNC":
		return "http://loinc.org"
	case "SCT", "SNM", "SNOMED", "SNOMEDCT", "SNOMED-CT":
		return "http://snomed.info/sct"
	case "RXNORM", "RXN":
		return "http://www.nlm.nih.gov/research/umls/rxnorm"
	case "CVX":
		return "http://hl7.org/fhir/sid/cvx"
	case "MVX":
		return "http://hl7.org/fhir/sid/mvx"
	case "NDC":
		return "http://hl7.org/fhir/sid/ndc"
	case "I10", "I10C", "ICD10", "ICD-10", "ICD-10-CM":
		return "http://hl7.org/fhir/sid/icd-10-cm"
	case "I9", "I9C", "ICD9", "ICD-9-CM":
		return "http://hl7.org/fhir/sid/icd-9-cm"
	case "C4", "CPT", "CPT4":
		return "http://www.ama-assn.org/go/cpt"
	case "UCUM":
		return "http://unitsofmeasure.org"
	case "NCIT":
		return "http://ncicb.nci.nih.gov/xml/owl/EVS/Thesaurus.owl"
	case "HL70162":
		return "http://terminology.hl7.org/CodeSystem/v2-0162"
	case "HL70163":
		return "http://terminology.hl7.org/CodeSystem/v2-0163"
	case "HL70063":
		return "http://terminology.hl7.org/CodeSystem/v2-0063"
	case "CDCREC":
		return "urn:oid:2.16.840.1.113883.6.238"
	default:
		if strings.HasPrefix(strings.ToUpper(s), "HL7") && len(s) == 7 && isAllDigits(s[3:]) {
			return "http://terminology.hl7.org/CodeSystem/v2-" + s[3:]
		}
		return defaultSystem
	}
}

func mapCEField(f Field, defaultSystem string) map[string]any {
	var codings []any
	var primaryText string
	for _, rep := range f.Repetitions {
		c1 := strings.TrimSpace(rep.Comp(1))
		t1 := strings.TrimSpace(rep.Comp(2))
		s1 := strings.TrimSpace(rep.Comp(3))
		if c1 != "" {
			cm := map[string]any{
				"system": mapCodeSystemURI(s1, defaultSystem),
				"code":   c1,
			}
			if t1 != "" {
				cm["display"] = t1
				if primaryText == "" {
					primaryText = t1
				}
			}
			codings = append(codings, cm)
		} else if t1 != "" && primaryText == "" {
			primaryText = t1
		}

		// Alternate coding triplet in CE/CWE components 4..6 (e.g. NDC alongside CVX, or local lab code alongside LOINC)
		c2 := strings.TrimSpace(rep.Comp(4))
		t2 := strings.TrimSpace(rep.Comp(5))
		s2 := strings.TrimSpace(rep.Comp(6))
		if c2 != "" {
			cm2 := map[string]any{
				"system": mapCodeSystemURI(s2, defaultSystem),
				"code":   c2,
			}
			if t2 != "" {
				cm2["display"] = t2
				if primaryText == "" {
					primaryText = t2
				}
			}
			codings = append(codings, cm2)
		}
		if origText := strings.TrimSpace(rep.Comp(9)); origText != "" && primaryText == "" {
			primaryText = origText
		}
	}

	out := map[string]any{}
	if len(codings) > 0 {
		out["coding"] = codings
	}
	if primaryText != "" {
		out["text"] = primaryText
	}
	return out
}

func mapCEFieldWithDefault(f Field, defaultSystem, fallbackText string) map[string]any {
	cc := mapCEField(f, defaultSystem)
	if len(cc) == 0 {
		return map[string]any{"text": fallbackText}
	}
	return cc
}

func mapXPNToHumanName(rep Repetition) map[string]any {
	family := strings.TrimSpace(rep.Comp(1))
	given := strings.TrimSpace(rep.Comp(2))
	middle := strings.TrimSpace(rep.Comp(3))
	suffix := strings.TrimSpace(rep.Comp(4))
	prefix := strings.TrimSpace(rep.Comp(5))
	nameType := strings.ToUpper(strings.TrimSpace(rep.Comp(7)))
	if family == "" && given == "" && middle == "" {
		return nil
	}
	out := map[string]any{}
	switch nameType {
	case "L", "D":
		out["use"] = "official"
	case "M", "P":
		out["use"] = "maiden"
	case "N":
		out["use"] = "nickname"
	case "A", "S":
		out["use"] = "anonymous"
	}
	if family != "" {
		out["family"] = family
	}
	var givens []any
	if given != "" {
		givens = append(givens, given)
	}
	if middle != "" {
		givens = append(givens, middle)
	}
	if len(givens) > 0 {
		out["given"] = givens
	}
	if prefix != "" {
		out["prefix"] = []any{prefix}
	}
	if suffix != "" {
		out["suffix"] = []any{suffix}
	}
	return out
}

func mapXADToAddress(rep Repetition) map[string]any {
	street1 := strings.TrimSpace(rep.Comp(1))
	street2 := strings.TrimSpace(rep.Comp(2))
	city := strings.TrimSpace(rep.Comp(3))
	state := strings.TrimSpace(rep.Comp(4))
	zip := strings.TrimSpace(rep.Comp(5))
	country := strings.TrimSpace(rep.Comp(6))
	addrType := strings.ToUpper(strings.TrimSpace(rep.Comp(7)))
	if street1 == "" && street2 == "" && city == "" && state == "" && zip == "" && country == "" {
		return nil
	}
	out := map[string]any{}
	var lines []any
	if street1 != "" {
		lines = append(lines, street1)
	}
	if street2 != "" {
		lines = append(lines, street2)
	}
	if len(lines) > 0 {
		out["line"] = lines
	}
	if city != "" {
		out["city"] = city
	}
	if state != "" {
		out["state"] = state
	}
	if zip != "" {
		out["postalCode"] = zip
	}
	if country != "" {
		out["country"] = country
	}
	switch addrType {
	case "H", "P":
		out["use"] = "home"
	case "B", "O":
		out["use"] = "work"
	case "C":
		out["use"] = "temp"
	case "BA":
		out["use"] = "billing"
	}
	return out
}

func mapXTNToContactPoint(rep Repetition, defaultUse string) map[string]any {
	useCode := strings.ToUpper(strings.TrimSpace(rep.Comp(2)))
	equipType := strings.ToUpper(strings.TrimSpace(rep.Comp(3)))
	email := strings.TrimSpace(rep.Comp(4))
	area := strings.TrimSpace(rep.Comp(6))
	local := strings.TrimSpace(rep.Comp(7))
	rawVal := strings.TrimSpace(rep.Comp(1))
	unformatted := strings.TrimSpace(rep.Comp(12))

	if equipType == "INTERNET" || (email != "" && strings.Contains(email, "@")) {
		val := firstNonEmptyStr(email, rawVal)
		if val == "" {
			return nil
		}
		return map[string]any{
			"system": "email",
			"value":  val,
			"use":    defaultUse,
		}
	}

	var phone string
	switch {
	case area != "" && local != "":
		if len(local) == 7 {
			phone = fmt.Sprintf("(%s) %s-%s", area, local[:3], local[3:])
		} else {
			phone = area + "-" + local
		}
	case rawVal != "":
		phone = rawVal
	case unformatted != "":
		phone = unformatted
	case local != "":
		phone = local
	}
	if phone == "" {
		return nil
	}
	use := defaultUse
	switch useCode {
	case "PRN":
		use = "home"
	case "WPN":
		use = "work"
	case "ORN":
		if equipType == "CP" {
			use = "mobile"
		}
	}
	if equipType == "CP" {
		use = "mobile"
	}
	return map[string]any{
		"system": "phone",
		"value":  phone,
		"use":    use,
	}
}

func mapNK1ToPatientContact(nk1 *Segment) map[string]any {
	if nk1 == nil {
		return nil
	}
	out := map[string]any{}
	if len(nk1.Field(2).Repetitions) > 0 {
		if nm := mapXPNToHumanName(nk1.Field(2).Repetitions[0]); nm != nil {
			out["name"] = nm
		}
	}
	if !nk1.Field(3).IsEmpty() {
		out["relationship"] = []any{
			mapCEField(nk1.Field(3), "http://terminology.hl7.org/CodeSystem/v2-0131"),
		}
	}
	var telecoms []any
	for _, rep := range nk1.Field(5).Repetitions {
		if tel := mapXTNToContactPoint(rep, "home"); tel != nil {
			telecoms = append(telecoms, tel)
		}
	}
	if len(telecoms) > 0 {
		out["telecom"] = telecoms
	}
	if len(nk1.Field(4).Repetitions) > 0 {
		if addr := mapXADToAddress(nk1.Field(4).Repetitions[0]); addr != nil {
			out["address"] = addr
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mapOMBRaceExtension(f Field) map[string]any {
	if f.IsEmpty() {
		return nil
	}
	var subExts []any
	var texts []string
	for _, rep := range f.Repetitions {
		code := strings.TrimSpace(rep.Comp(1))
		disp := firstNonEmptyStr(strings.TrimSpace(rep.Comp(2)), ombRaceDisplay(code))
		if isOMBRaceCategory(code) {
			subExts = append(subExts, map[string]any{
				"url": "ombCategory",
				"valueCoding": map[string]any{
					"system":  "urn:oid:2.16.840.1.113883.6.238",
					"code":    code,
					"display": disp,
				},
			})
		}
		if disp != "" {
			texts = append(texts, disp)
		}
	}
	if len(subExts) == 0 && len(texts) == 0 {
		return nil
	}
	txt := strings.Join(texts, ", ")
	if txt == "" {
		txt = "Unknown"
	}
	subExts = append(subExts, map[string]any{
		"url":         "text",
		"valueString": txt,
	})
	return map[string]any{
		"url":       "http://hl7.org/fhir/us/core/StructureDefinition/us-core-race",
		"extension": subExts,
	}
}

func mapOMBEthnicityExtension(f Field) map[string]any {
	if f.IsEmpty() {
		return nil
	}
	code := strings.TrimSpace(f.ValComp(1))
	disp := strings.TrimSpace(f.ValComp(2))
	switch strings.ToUpper(code) {
	case "H", "2135-2":
		code = "2135-2"
		if disp == "" {
			disp = "Hispanic or Latino"
		}
	case "N", "2186-5":
		code = "2186-5"
		if disp == "" {
			disp = "Not Hispanic or Latino"
		}
	default:
		if disp == "" {
			return nil
		}
	}
	var subExts []any
	if code == "2135-2" || code == "2186-5" {
		subExts = append(subExts, map[string]any{
			"url": "ombCategory",
			"valueCoding": map[string]any{
				"system":  "urn:oid:2.16.840.1.113883.6.238",
				"code":    code,
				"display": disp,
			},
		})
	}
	subExts = append(subExts, map[string]any{
		"url":         "text",
		"valueString": disp,
	})
	return map[string]any{
		"url":       "http://hl7.org/fhir/us/core/StructureDefinition/us-core-ethnicity",
		"extension": subExts,
	}
}

// ValComp reads component idx of the first repetition.
func (f Field) ValComp(idx int) string {
	return f.Comp(idx)
}

func isOMBRaceCategory(code string) bool {
	switch code {
	case "1002-5", "2028-9", "2054-5", "2076-8", "2106-3":
		return true
	default:
		return false
	}
}

func ombRaceDisplay(code string) string {
	switch code {
	case "1002-5":
		return "American Indian or Alaska Native"
	case "2028-9":
		return "Asian"
	case "2054-5":
		return "Black or African American"
	case "2076-8":
		return "Native Hawaiian or Other Pacific Islander"
	case "2106-3":
		return "White"
	default:
		return ""
	}
}

func mapHL7Gender(raw string) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "M", "MALE":
		return "male"
	case "F", "FEMALE":
		return "female"
	case "O", "OTHER":
		return "other"
	case "U", "A", "N", "UNK", "UNKNOWN":
		return "unknown"
	default:
		return ""
	}
}

func mapMaritalStatus(code string) string {
	switch code {
	case "S":
		return "S"
	case "M":
		return "M"
	case "D":
		return "D"
	case "W":
		return "W"
	case "A":
		return "A"
	case "L":
		return "L"
	case "P":
		return "P"
	case "U":
		return "UNK"
	default:
		return ""
	}
}

func mapPatientClass(pv1Class, trigger string) (code, display string) {
	switch pv1Class {
	case "I":
		return "IMP", "inpatient encounter"
	case "O":
		return "AMB", "ambulatory"
	case "E":
		return "EMER", "emergency"
	case "P":
		return "PRENC", "pre-admission"
	case "R":
		return "IMP", "recurring inpatient"
	case "H":
		return "HH", "home health"
	}
	if trigger == "A01" || trigger == "A03" {
		return "IMP", "inpatient encounter"
	}
	return "AMB", "ambulatory"
}

func mapOBRStatus(code string) string {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "F":
		return "final"
	case "P", "R", "S":
		return "preliminary"
	case "C", "M":
		return "corrected"
	case "X":
		return "cancelled"
	case "I", "O", "A":
		return "registered"
	default:
		return "final"
	}
}

func mapOBXStatus(code string) string {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "F":
		return "final"
	case "P", "R", "S":
		return "preliminary"
	case "C":
		return "corrected"
	case "A":
		return "amended"
	case "X", "D":
		return "cancelled"
	case "W":
		return "entered-in-error"
	case "I", "O":
		return "registered"
	default:
		return "final"
	}
}

func mapORMOrderStatus(orderCtrl, orderStatus string) string {
	switch orderCtrl {
	case "CA", "DC", "OC":
		return "revoked"
	case "HD", "OH":
		return "on-hold"
	}
	switch orderStatus {
	case "CA", "DC":
		return "revoked"
	case "HD":
		return "on-hold"
	case "CM":
		return "completed"
	case "ER":
		return "entered-in-error"
	default:
		return "active"
	}
}

func mapOrderPriority(raw string) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "S", "STAT":
		return "stat"
	case "A", "ASAP":
		return "asap"
	case "T", "P", "U", "URGENT":
		return "urgent"
	case "R", "ROUTINE":
		return "routine"
	default:
		return ""
	}
}

func extractOrderKey(orc, obr *Segment, fallback string, seq int) string {
	var placer, filler string
	if obr != nil {
		placer = strings.TrimSpace(obr.Val(2, 1))
		filler = strings.TrimSpace(obr.Val(3, 1))
	}
	if orc != nil {
		if filler == "" {
			filler = strings.TrimSpace(orc.Val(3, 1))
		}
		if placer == "" {
			placer = strings.TrimSpace(orc.Val(2, 1))
		}
	}
	if filler != "" {
		return "filler:" + filler
	}
	if placer != "" {
		return "placer:" + placer
	}
	if fallback == "" {
		return ""
	}
	return fmt.Sprintf("%s:ord:%d", fallback, seq)
}

func buildOrderIdentifiers(orc, obr *Segment, sysPrefix string) []any {
	var out []any
	var placer, placerAuth, filler, fillerAuth string
	if obr != nil {
		placer = strings.TrimSpace(obr.Val(2, 1))
		placerAuth = firstNonEmptyStr(obr.Val(2, 3), obr.Val(2, 2))
		filler = strings.TrimSpace(obr.Val(3, 1))
		fillerAuth = firstNonEmptyStr(obr.Val(3, 3), obr.Val(3, 2))
	}
	if orc != nil {
		if placer == "" {
			placer = strings.TrimSpace(orc.Val(2, 1))
			placerAuth = firstNonEmptyStr(orc.Val(2, 3), orc.Val(2, 2))
		}
		if filler == "" {
			filler = strings.TrimSpace(orc.Val(3, 1))
			fillerAuth = firstNonEmptyStr(orc.Val(3, 3), orc.Val(3, 2))
		}
	}
	if placer != "" {
		out = append(out, map[string]any{
			"system": normalizeAssigningAuthority(placerAuth, sysPrefix+"/placer-order"),
			"value":  placer,
			"type": map[string]any{
				"coding": []any{
					map[string]any{
						"system": "http://terminology.hl7.org/CodeSystem/v2-0203",
						"code":   "PLAC",
					},
				},
			},
		})
	}
	if filler != "" {
		out = append(out, map[string]any{
			"system": normalizeAssigningAuthority(fillerAuth, sysPrefix+"/filler-order"),
			"value":  filler,
			"type": map[string]any{
				"coding": []any{
					map[string]any{
						"system": "http://terminology.hl7.org/CodeSystem/v2-0203",
						"code":   "FILL",
					},
				},
			},
		})
	}
	return out
}

func applyUCUMUnits(q map[string]any, code, text string) {
	uText := firstNonEmptyStr(text, code)
	if uText != "" {
		q["unit"] = uText
	}
	if code != "" {
		q["system"] = "http://unitsofmeasure.org"
		q["code"] = code
	}
}

func parseReferenceRange(raw string, unitField Field) map[string]any {
	out := map[string]any{"text": raw}
	uCode := strings.TrimSpace(unitField.Comp(1))
	uText := firstNonEmptyStr(strings.TrimSpace(unitField.Comp(2)), uCode)
	// Handle "low-high" or "low - high"
	if idx := strings.IndexByte(raw[min(1, len(raw)):], '-'); idx >= 0 {
		splitAt := idx + 1
		left := strings.TrimSpace(raw[:splitAt])
		right := strings.TrimSpace(raw[splitAt+1:])
		lVal, errL := strconv.ParseFloat(left, 64)
		rVal, errR := strconv.ParseFloat(right, 64)
		if errL == nil && errR == nil {
			lowQ := map[string]any{"value": lVal}
			highQ := map[string]any{"value": rVal}
			applyUCUMUnits(lowQ, uCode, uText)
			applyUCUMUnits(highQ, uCode, uText)
			out["low"] = lowQ
			out["high"] = highQ
		}
	}
	return out
}

func splitLeadingComparator(s string) (comp string, num string) {
	for _, p := range []string{"<=", ">=", "<", ">"} {
		if strings.HasPrefix(s, p) {
			return p, strings.TrimSpace(s[len(p):])
		}
	}
	return "", s
}

func extractSingleNTE(nte *Segment) string {
	if nte == nil {
		return ""
	}
	var parts []string
	for _, rep := range nte.Field(3).Repetitions {
		if v := strings.TrimSpace(rep.Value()); v != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, "\n")
}

func joinNTEText(ntes []*Segment) string {
	var parts []string
	for _, nte := range ntes {
		if t := extractSingleNTE(nte); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

func formatHL7DateTime(raw string) (string, bool) {
	return ParseTimestamp(raw, false)
}

func formatHL7Instant(raw string) (string, bool) {
	dt, ok := ParseTimestamp(raw, false)
	if !ok {
		return "", false
	}
	if len(dt) == 10 {
		return dt + "T00:00:00Z", true
	}
	if len(dt) == 7 {
		return dt + "-01T00:00:00Z", true
	}
	if len(dt) == 4 {
		return dt + "-01-01T00:00:00Z", true
	}
	return dt, true
}

func formatHL7InstantOrFallback(raw string, fallback time.Time) string {
	if inst, ok := formatHL7Instant(raw); ok {
		return inst
	}
	return fallback.UTC().Format(time.RFC3339)
}

func formatHL7Time(raw string) string {
	s := strings.TrimSpace(raw)
	if len(s) < 4 || !isAllDigits(s[:4]) {
		return ""
	}
	hh := s[:2]
	mm := s[2:4]
	if atoi2(hh) > 23 || atoi2(mm) > 59 {
		return ""
	}
	ss := "00"
	if len(s) >= 6 && isAllDigits(s[4:6]) {
		if atoi2(s[4:6]) <= 59 {
			ss = s[4:6]
		}
	}
	return hh + ":" + mm + ":" + ss
}

func normalizeLanguageBCP47(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "eng", "en":
		return "en"
	case "spa", "es":
		return "es"
	case "fra", "fre", "fr":
		return "fr"
	case "zho", "chi", "zh":
		return "zh"
	default:
		return strings.ToLower(strings.TrimSpace(raw))
	}
}

func isOID(s string) bool {
	if len(s) < 3 || !strings.Contains(s, ".") {
		return false
	}
	if s[0] < '0' || s[0] > '2' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '.' && (s[i] < '0' || s[i] > '9') {
			return false
		}
	}
	return !strings.HasPrefix(s, ".") && !strings.HasSuffix(s, ".") && !strings.Contains(s, "..")
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}
