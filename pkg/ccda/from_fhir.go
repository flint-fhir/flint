package ccda

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ExportOptions configures FHIR R4 -> HL7 C-CDA R2.1 CCD generation.
type ExportOptions struct {
	DocumentID    string
	Title         string
	CustodianName string
	AuthorName    string
	Now           time.Time
}

var vitalSignLOINCs = map[string]bool{
	"85353-1": true,
	"85354-9": true,
	"96607-7": true,
	"8480-6":  true,
	"8462-4":  true,
	"96608-5": true,
	"96609-3": true,
	"8867-4":  true,
	"9279-1":  true,
	"8310-5":  true,
	"8302-2":  true,
	"29463-7": true,
	"39156-5": true,
	"2708-6":  true,
	"59408-5": true,
}

// ConvertBundleToCCDA converts a FHIR R4 Bundle (IPS, document, searchset, or transaction)
// containing a Patient and clinical compartment resources into an HL7 C-CDA R2.1 CCD XML document.
func ConvertBundleToCCDA(bundle map[string]any, opts ExportOptions) ([]byte, error) {
	if bundle == nil {
		return nil, ErrEmptyDocument
	}
	entries, _ := bundle["entry"].([]any)
	var patient map[string]any
	var resources []map[string]any
	for _, rawEntry := range entries {
		em, ok := rawEntry.(map[string]any)
		if !ok {
			continue
		}
		res, ok := em["resource"].(map[string]any)
		if !ok {
			continue
		}
		rt, _ := res["resourceType"].(string)
		if rt == "Patient" && patient == nil {
			patient = res
		} else {
			resources = append(resources, res)
		}
	}
	if patient == nil {
		return nil, fmt.Errorf("%w: bundle contains no Patient resource", ErrInvalidCCDA)
	}
	return ConvertFHIRToCCDA(patient, resources, opts)
}

// ConvertFHIRToCCDA converts a FHIR R4 Patient and slice of compartment resources into
// a schema-structured HL7 C-CDA R2.1 Continuity of Care Document (CCD) XML document.
func ConvertFHIRToCCDA(patient map[string]any, resources []map[string]any, opts ExportOptions) ([]byte, error) {
	if patient == nil {
		return nil, fmt.Errorf("%w: patient map is required", ErrInvalidCCDA)
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	patID, _ := patient["id"].(string)
	docID := strings.TrimSpace(opts.DocumentID)
	if docID == "" {
		docID = deterministicID("ccd", firstStr(patID, "patient"), now.Format("20060102150405"))
	}
	title := firstStr(strings.TrimSpace(opts.Title), "Continuity of Care Document (C-CDA R2.1)")
	custName := firstStr(strings.TrimSpace(opts.CustodianName), "Flint Clinical Exchange")
	authName := firstStr(strings.TrimSpace(opts.AuthorName), "Flint FHIR-to-CCDA Bridge")

	var (
		conditions []*map[string]any
		allergies  []*map[string]any
		meds       []*map[string]any
		immuns     []*map[string]any
		labs       []*map[string]any
		reports    []*map[string]any
		vitals     []*map[string]any
		procs      []*map[string]any
		encs       []*map[string]any
		firstPrac  map[string]any
		firstOrg   map[string]any
	)
	obsByID := make(map[string]map[string]any)

	for i := range resources {
		res := resources[i]
		if res == nil {
			continue
		}
		rt, _ := res["resourceType"].(string)
		switch rt {
		case "Condition":
			c := res
			conditions = append(conditions, &c)
		case "AllergyIntolerance":
			a := res
			allergies = append(allergies, &a)
		case "MedicationRequest", "MedicationStatement":
			m := res
			meds = append(meds, &m)
		case "Immunization":
			im := res
			immuns = append(immuns, &im)
		case "Procedure":
			p := res
			procs = append(procs, &p)
		case "Encounter":
			e := res
			encs = append(encs, &e)
		case "DiagnosticReport":
			dr := res
			reports = append(reports, &dr)
		case "Observation":
			if idStr, _ := res["id"].(string); idStr != "" {
				obsByID[idStr] = res
				obsByID["Observation/"+idStr] = res
			}
			o := res
			if isVitalObservation(res) {
				vitals = append(vitals, &o)
			} else {
				labs = append(labs, &o)
			}
		case "Practitioner":
			if firstPrac == nil {
				firstPrac = res
			}
		case "Organization":
			if firstOrg == nil {
				firstOrg = res
			}
		}
	}

	if firstOrg != nil {
		if nm, _ := firstOrg["name"].(string); strings.TrimSpace(nm) != "" {
			custName = strings.TrimSpace(nm)
		}
	}

	var buf bytes.Buffer
	buf.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	buf.WriteString(`<ClinicalDocument xmlns="urn:hl7-org:v3" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:sdtc="urn:hl7-org:sdtc">` + "\n")
	buf.WriteString(`  <realmCode code="US"/>` + "\n")
	buf.WriteString(`  <typeId root="2.16.840.1.113883.1.3" extension="POCD_HD000040"/>` + "\n")
	buf.WriteString(`  <templateId root="2.16.840.1.113883.10.20.22.1.1"/>` + "\n")
	buf.WriteString(`  <templateId root="2.16.840.1.113883.10.20.22.1.1" extension="2015-08-01"/>` + "\n")
	buf.WriteString(`  <templateId root="2.16.840.1.113883.10.20.22.1.2"/>` + "\n")
	buf.WriteString(`  <templateId root="2.16.840.1.113883.10.20.22.1.2" extension="2015-08-01"/>` + "\n")
	fmt.Fprintf(&buf, `  <id root="2.16.840.1.113883.19.5.99999.1" extension="%s"/>`+"\n", escAttr(docID))
	buf.WriteString(`  <code code="34133-9" codeSystem="2.16.840.1.113883.6.1" codeSystemName="LOINC" displayName="Summarization of Episode Note"/>` + "\n")
	fmt.Fprintf(&buf, `  <title>%s</title>`+"\n", escText(title))
	fmt.Fprintf(&buf, `  <effectiveTime value="%s"/>`+"\n", escAttr(FormatFHIRToCDATimestamp(now.Format(time.RFC3339))))
	buf.WriteString(`  <confidentialityCode code="N" codeSystem="2.16.840.1.113883.5.25" displayName="normal"/>` + "\n")
	buf.WriteString(`  <languageCode code="en-US"/>` + "\n")

	// recordTarget
	writeCCDARecordTarget(&buf, patient, custName)

	// author
	writeCCDAAuthor(&buf, firstPrac, authName, custName, now)

	// custodian
	buf.WriteString("  <custodian>\n    <assignedCustodian>\n      <representedCustodianOrganization>\n")
	buf.WriteString(`        <id root="2.16.840.1.113883.4.6"/>` + "\n")
	fmt.Fprintf(&buf, "        <name>%s</name>\n", escText(custName))
	buf.WriteString("      </representedCustodianOrganization>\n    </assignedCustodian>\n  </custodian>\n")

	// structuredBody with all 8 canonical C-CDA R2.1 sections
	buf.WriteString("  <component>\n    <structuredBody>\n")

	writeProblemsSection(&buf, conditions)
	writeAllergiesSection(&buf, allergies)
	writeMedicationsSection(&buf, meds)
	writeImmunizationsSection(&buf, immuns)
	writeResultsSection(&buf, reports, labs, obsByID, now)
	writeVitalsSection(&buf, vitals, now)
	writeProceduresSection(&buf, procs)
	writeEncountersSection(&buf, encs)

	buf.WriteString("    </structuredBody>\n  </component>\n</ClinicalDocument>\n")
	return buf.Bytes(), nil
}

func writeCCDARecordTarget(buf *bytes.Buffer, pat map[string]any, providerOrgName string) {
	buf.WriteString("  <recordTarget>\n    <patientRole>\n")
	wroteID := false
	if ids, ok := pat["identifier"].([]any); ok {
		for _, item := range ids {
			im, ok := item.(map[string]any)
			if !ok {
				continue
			}
			val, _ := im["value"].(string)
			if strings.TrimSpace(val) == "" {
				continue
			}
			sys, _ := im["system"].(string)
			oid, _ := FHIRSystemToOID(sys)
			if !isOID(oid) {
				oid = "2.16.840.1.113883.19.5"
			}
			fmt.Fprintf(buf, `      <id root="%s" extension="%s"/>`+"\n", escAttr(oid), escAttr(val))
			wroteID = true
		}
	}
	if patID, _ := pat["id"].(string); !wroteID && patID != "" {
		fmt.Fprintf(buf, `      <id root="2.16.840.1.113883.19.5" extension="%s"/>`+"\n", escAttr(patID))
		wroteID = true
	}
	if !wroteID {
		buf.WriteString(`      <id nullFlavor="NI"/>` + "\n")
	}

	if addrs, ok := pat["address"].([]any); ok {
		for _, a := range addrs {
			if am, ok := a.(map[string]any); ok {
				writeCDAAd(buf, "      ", am)
			}
		}
	}
	if tels, ok := pat["telecom"].([]any); ok {
		for _, t := range tels {
			if tm, ok := t.(map[string]any); ok {
				writeCDATel(buf, "      ", tm)
			}
		}
	}

	buf.WriteString("      <patient>\n")
	if names, ok := pat["name"].([]any); ok {
		for _, n := range names {
			if nm, ok := n.(map[string]any); ok {
				writeCDAName(buf, "        ", nm)
			}
		}
	}
	if gStr, _ := pat["gender"].(string); gStr != "" {
		cdaG, disp := fhirGenderToCDA(gStr)
		fmt.Fprintf(buf, `        <administrativeGenderCode code="%s" codeSystem="2.16.840.1.113883.5.1" displayName="%s"/>`+"\n", escAttr(cdaG), escAttr(disp))
	}
	if bd, _ := pat["birthDate"].(string); bd != "" {
		if ts := FormatFHIRToCDATimestamp(bd); ts != "" {
			fmt.Fprintf(buf, `        <birthTime value="%s"/>`+"\n", escAttr(ts))
		}
	}
	if ms, ok := pat["maritalStatus"].(map[string]any); ok {
		writeCDACodedElement(buf, "        ", "maritalStatusCode", ms, "2.16.840.1.113883.5.2")
	}
	writeRaceAndEthnicityFromPatient(buf, pat)
	if comms, ok := pat["communication"].([]any); ok {
		for _, c := range comms {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			langCode := firstCodingCode(cm["language"])
			if langCode != "" {
				buf.WriteString("        <languageCommunication>\n")
				fmt.Fprintf(buf, `          <languageCode code="%s"/>`+"\n", escAttr(langCode))
				if pref, ok := cm["preferred"].(bool); ok && pref {
					buf.WriteString(`          <preferenceInd value="true"/>` + "\n")
				}
				buf.WriteString("        </languageCommunication>\n")
			}
		}
	}
	if decDT, _ := pat["deceasedDateTime"].(string); decDT != "" {
		if ts := FormatFHIRToCDATimestamp(decDT); ts != "" {
			buf.WriteString(`        <sdtc:deceasedInd value="true"/>` + "\n")
			fmt.Fprintf(buf, `        <sdtc:deceasedTime value="%s"/>`+"\n", escAttr(ts))
		}
	} else if decBool, ok := pat["deceasedBoolean"].(bool); ok && decBool {
		buf.WriteString(`        <sdtc:deceasedInd value="true"/>` + "\n")
	}
	buf.WriteString("      </patient>\n")
	if providerOrgName != "" {
		buf.WriteString("      <providerOrganization>\n")
		buf.WriteString(`        <id root="2.16.840.1.113883.4.6"/>` + "\n")
		fmt.Fprintf(buf, "        <name>%s</name>\n", escText(providerOrgName))
		buf.WriteString("      </providerOrganization>\n")
	}
	buf.WriteString("    </patientRole>\n  </recordTarget>\n")
}

func writeRaceAndEthnicityFromPatient(buf *bytes.Buffer, pat map[string]any) {
	exts, _ := pat["extension"].([]any)
	var wroteRace, wroteEth bool
	for _, rawExt := range exts {
		em, ok := rawExt.(map[string]any)
		if !ok {
			continue
		}
		urlStr, _ := em["url"].(string)
		subExts, _ := em["extension"].([]any)
		switch urlStr {
		case "http://hl7.org/fhir/us/core/StructureDefinition/us-core-race":
			for _, sub := range subExts {
				sm, ok := sub.(map[string]any)
				if !ok {
					continue
				}
				vc, ok := sm["valueCoding"].(map[string]any)
				if !ok {
					continue
				}
				code, _ := vc["code"].(string)
				disp, _ := vc["display"].(string)
				if code == "" {
					continue
				}
				tag := "raceCode"
				if wroteRace {
					tag = "sdtc:raceCode"
				}
				wroteRace = true
				fmt.Fprintf(buf, `        <%s code="%s" codeSystem="2.16.840.1.113883.6.238" displayName="%s"/>`+"\n", tag, escAttr(code), escAttr(disp))
			}
		case "http://hl7.org/fhir/us/core/StructureDefinition/us-core-ethnicity":
			for _, sub := range subExts {
				sm, ok := sub.(map[string]any)
				if !ok {
					continue
				}
				vc, ok := sm["valueCoding"].(map[string]any)
				if !ok {
					continue
				}
				code, _ := vc["code"].(string)
				disp, _ := vc["display"].(string)
				if code == "" {
					continue
				}
				tag := "ethnicGroupCode"
				if wroteEth {
					tag = "sdtc:ethnicGroupCode"
				}
				wroteEth = true
				fmt.Fprintf(buf, `        <%s code="%s" codeSystem="2.16.840.1.113883.6.238" displayName="%s"/>`+"\n", tag, escAttr(code), escAttr(disp))
			}
		}
	}
	if !wroteRace {
		buf.WriteString(`        <raceCode nullFlavor="UNK"/>` + "\n")
	}
	if !wroteEth {
		buf.WriteString(`        <ethnicGroupCode nullFlavor="UNK"/>` + "\n")
	}
}

func writeCCDAAuthor(buf *bytes.Buffer, prac map[string]any, fallbackAuthorName, orgName string, now time.Time) {
	buf.WriteString("  <author>\n")
	fmt.Fprintf(buf, `    <time value="%s"/>`+"\n", escAttr(FormatFHIRToCDATimestamp(now.Format(time.RFC3339))))
	buf.WriteString("    <assignedAuthor>\n")
	buf.WriteString(`      <id root="2.16.840.1.113883.4.6" extension="1234567890"/>` + "\n")
	buf.WriteString("      <assignedPerson>\n")
	if prac != nil {
		if names, ok := prac["name"].([]any); ok && len(names) > 0 {
			if nm, ok := names[0].(map[string]any); ok {
				writeCDAName(buf, "        ", nm)
			}
		} else {
			fmt.Fprintf(buf, "        <name><given>%s</given></name>\n", escText(fallbackAuthorName))
		}
	} else {
		fmt.Fprintf(buf, "        <name><given>%s</given></name>\n", escText(fallbackAuthorName))
	}
	buf.WriteString("      </assignedPerson>\n")
	if orgName != "" {
		buf.WriteString("      <representedOrganization>\n")
		fmt.Fprintf(buf, "        <name>%s</name>\n", escText(orgName))
		buf.WriteString("      </representedOrganization>\n")
	}
	buf.WriteString("    </assignedAuthor>\n  </author>\n")
}

func writeProblemsSection(buf *bytes.Buffer, conditions []*map[string]any) {
	writeSectionStart(buf, SectionProblems, SectionProblemsEntries, "11450-4", "Problem List", "Problems", len(conditions) == 0)
	if len(conditions) == 0 {
		buf.WriteString("        <text><paragraph>No known active problems recorded.</paragraph></text>\n")
		buf.WriteString("      </section>\n    </component>\n")
		return
	}
	buf.WriteString("        <text><table><thead><tr><th>Condition</th><th>Status</th><th>Onset</th></tr></thead><tbody>\n")
	for _, cPtr := range conditions {
		c := *cPtr
		label := displayForCC(c["code"])
		st := firstCodingCode(c["clinicalStatus"])
		onset, _ := c["onsetDateTime"].(string)
		fmt.Fprintf(buf, "          <tr><td>%s</td><td>%s</td><td>%s</td></tr>\n", escText(label), escText(st), escText(onset))
	}
	buf.WriteString("        </tbody></table></text>\n")

	for idx, cPtr := range conditions {
		c := *cPtr
		idStr, _ := c["id"].(string)
		if idStr == "" {
			idStr = fmt.Sprintf("prob-%d", idx+1)
		}
		st := firstCodingCode(c["clinicalStatus"])
		actStatus := "active"
		if st == "resolved" || st == "inactive" {
			actStatus = "completed"
		}
		buf.WriteString("        <entry typeCode=\"DRIV\">\n")
		buf.WriteString("          <act classCode=\"ACT\" moodCode=\"EVN\">\n")
		buf.WriteString(`            <templateId root="2.16.840.1.113883.10.20.22.4.3"/>` + "\n")
		buf.WriteString(`            <templateId root="2.16.840.1.113883.10.20.22.4.3" extension="2015-08-01"/>` + "\n")
		fmt.Fprintf(buf, `            <id root="2.16.840.1.113883.19.5" extension="%s-concern"/>`+"\n", escAttr(idStr))
		buf.WriteString(`            <code code="CONC" codeSystem="2.16.840.1.113883.5.6" displayName="Concern"/>` + "\n")
		fmt.Fprintf(buf, `            <statusCode code="%s"/>`+"\n", escAttr(actStatus))
		writeIntervalEffectiveTime(buf, "            ", strVal(c["onsetDateTime"]), strVal(c["abatementDateTime"]))
		buf.WriteString("            <entryRelationship typeCode=\"SUBJ\">\n")
		buf.WriteString("              <observation classCode=\"OBS\" moodCode=\"EVN\">\n")
		buf.WriteString(`                <templateId root="2.16.840.1.113883.10.20.22.4.4"/>` + "\n")
		buf.WriteString(`                <templateId root="2.16.840.1.113883.10.20.22.4.4" extension="2015-08-01"/>` + "\n")
		fmt.Fprintf(buf, `                <id root="2.16.840.1.113883.19.5" extension="%s"/>`+"\n", escAttr(idStr))
		buf.WriteString(`                <code code="55607006" codeSystem="2.16.840.1.113883.6.96" codeSystemName="SNOMED CT" displayName="Problem"><translation code="75326-9" codeSystem="2.16.840.1.113883.6.1" codeSystemName="LOINC" displayName="Problem"/></code>` + "\n")
		buf.WriteString(`                <statusCode code="completed"/>` + "\n")
		writeIntervalEffectiveTime(buf, "                ", strVal(c["onsetDateTime"]), strVal(c["abatementDateTime"]))
		writeCDACodedValue(buf, "                ", c["code"], "2.16.840.1.113883.6.96")
		buf.WriteString("              </observation>\n            </entryRelationship>\n          </act>\n        </entry>\n")
	}
	buf.WriteString("      </section>\n    </component>\n")
}

func writeAllergiesSection(buf *bytes.Buffer, allergies []*map[string]any) {
	writeSectionStart(buf, SectionAllergies, SectionAllergiesEntries, "48765-2", "Allergies and adverse reactions Document", "Allergies and Intolerances", len(allergies) == 0)
	if len(allergies) == 0 {
		buf.WriteString("        <text><paragraph>No known allergies recorded.</paragraph></text>\n")
		buf.WriteString("      </section>\n    </component>\n")
		return
	}
	buf.WriteString("        <text><table><thead><tr><th>Substance</th><th>Criticality</th><th>Reaction</th></tr></thead><tbody>\n")
	for _, aPtr := range allergies {
		a := *aPtr
		fmt.Fprintf(buf, "          <tr><td>%s</td><td>%s</td><td>%s</td></tr>\n",
			escText(displayForCC(a["code"])), escText(strVal(a["criticality"])), escText(firstReactionDisplay(a)))
	}
	buf.WriteString("        </tbody></table></text>\n")

	for idx, aPtr := range allergies {
		a := *aPtr
		idStr, _ := a["id"].(string)
		if idStr == "" {
			idStr = fmt.Sprintf("allergy-%d", idx+1)
		}
		buf.WriteString("        <entry typeCode=\"DRIV\">\n")
		buf.WriteString("          <act classCode=\"ACT\" moodCode=\"EVN\">\n")
		buf.WriteString(`            <templateId root="2.16.840.1.113883.10.20.22.4.30"/>` + "\n")
		buf.WriteString(`            <templateId root="2.16.840.1.113883.10.20.22.4.30" extension="2015-08-01"/>` + "\n")
		fmt.Fprintf(buf, `            <id root="2.16.840.1.113883.19.5" extension="%s-concern"/>`+"\n", escAttr(idStr))
		buf.WriteString(`            <code code="CONC" codeSystem="2.16.840.1.113883.5.6"/>` + "\n")
		buf.WriteString(`            <statusCode code="active"/>` + "\n")
		writeIntervalEffectiveTime(buf, "            ", strVal(a["onsetDateTime"]), "")
		buf.WriteString("            <entryRelationship typeCode=\"SUBJ\">\n")
		buf.WriteString("              <observation classCode=\"OBS\" moodCode=\"EVN\">\n")
		buf.WriteString(`                <templateId root="2.16.840.1.113883.10.20.22.4.7"/>` + "\n")
		buf.WriteString(`                <templateId root="2.16.840.1.113883.10.20.22.4.7" extension="2014-06-09"/>` + "\n")
		fmt.Fprintf(buf, `                <id root="2.16.840.1.113883.19.5" extension="%s"/>`+"\n", escAttr(idStr))
		buf.WriteString(`                <code code="ASSERTION" codeSystem="2.16.840.1.113883.5.4"/>` + "\n")
		buf.WriteString(`                <statusCode code="completed"/>` + "\n")
		writeIntervalEffectiveTime(buf, "                ", strVal(a["onsetDateTime"]), "")

		catCode, catDisp := "419199007", "Allergy to substance"
		if cats, ok := a["category"].([]any); ok && len(cats) > 0 {
			switch strVal(cats[0]) {
			case "medication":
				catCode, catDisp = "416098002", "Drug allergy"
			case "food":
				catCode, catDisp = "414285001", "Food allergy"
			}
		}
		fmt.Fprintf(buf, `                <value xsi:type="CD" code="%s" codeSystem="2.16.840.1.113883.6.96" displayName="%s"/>`+"\n", catCode, escAttr(catDisp))
		buf.WriteString("                <participant typeCode=\"CSM\">\n                  <participantRole classCode=\"MANU\">\n                    <playingEntity classCode=\"MMAT\">\n")
		writeCDACodedElement(buf, "                      ", "code", a["code"], "2.16.840.1.113883.6.88")
		buf.WriteString("                    </playingEntity>\n                  </participantRole>\n                </participant>\n")

		if crit := strVal(a["criticality"]); crit != "" {
			ccCode, ccDisp := "CRITL", "Low Criticality"
			if crit == "high" {
				ccCode, ccDisp = "CRITH", "High Criticality"
			}
			buf.WriteString("                <entryRelationship typeCode=\"SUBJ\" inversionInd=\"true\">\n                  <observation classCode=\"OBS\" moodCode=\"EVN\">\n")
			buf.WriteString(`                    <templateId root="2.16.840.1.113883.10.20.22.4.145"/>` + "\n")
			buf.WriteString(`                    <code code="82606-5" codeSystem="2.16.840.1.113883.6.1" displayName="Criticality"/>` + "\n")
			buf.WriteString(`                    <statusCode code="completed"/>` + "\n")
			fmt.Fprintf(buf, `                    <value xsi:type="CD" code="%s" codeSystem="2.16.840.1.113883.5.1063" displayName="%s"/>`+"\n", ccCode, ccDisp)
			buf.WriteString("                  </observation>\n                </entryRelationship>\n")
		}
		if rxns, ok := a["reaction"].([]any); ok && len(rxns) > 0 {
			if rxn, ok := rxns[0].(map[string]any); ok {
				if manifs, ok := rxn["manifestation"].([]any); ok && len(manifs) > 0 {
					buf.WriteString("                <entryRelationship typeCode=\"MFST\" inversionInd=\"true\">\n                  <observation classCode=\"OBS\" moodCode=\"EVN\">\n")
					buf.WriteString(`                    <templateId root="2.16.840.1.113883.10.20.22.4.9"/>` + "\n")
					buf.WriteString(`                    <code code="ASSERTION" codeSystem="2.16.840.1.113883.5.4"/>` + "\n")
					buf.WriteString(`                    <statusCode code="completed"/>` + "\n")
					writeCDACodedValue(buf, "                    ", manifs[0], "2.16.840.1.113883.6.96")
					if sev := strVal(rxn["severity"]); sev != "" {
						sCode, sDisp := "255604002", "Mild"
						switch sev {
						case "severe":
							sCode, sDisp = "24484000", "Severe"
						case "moderate":
							sCode, sDisp = "6736007", "Moderate"
						}
						buf.WriteString("                    <entryRelationship typeCode=\"SUBJ\" inversionInd=\"true\">\n                      <observation classCode=\"OBS\" moodCode=\"EVN\">\n")
						buf.WriteString(`                        <templateId root="2.16.840.1.113883.10.20.22.4.8"/>` + "\n")
						buf.WriteString(`                        <code code="SEV" codeSystem="2.16.840.1.113883.5.4"/>` + "\n")
						buf.WriteString(`                        <statusCode code="completed"/>` + "\n")
						fmt.Fprintf(buf, `                        <value xsi:type="CD" code="%s" codeSystem="2.16.840.1.113883.6.96" displayName="%s"/>`+"\n", sCode, sDisp)
						buf.WriteString("                      </observation>\n                    </entryRelationship>\n")
					}
					buf.WriteString("                  </observation>\n                </entryRelationship>\n")
				}
			}
		}
		buf.WriteString("              </observation>\n            </entryRelationship>\n          </act>\n        </entry>\n")
	}
	buf.WriteString("      </section>\n    </component>\n")
}

func writeMedicationsSection(buf *bytes.Buffer, meds []*map[string]any) {
	writeSectionStart(buf, SectionMeds, SectionMedsEntries, "10160-0", "History of Medication use Narrative", "Medications", len(meds) == 0)
	if len(meds) == 0 {
		buf.WriteString("        <text><paragraph>No active medications recorded.</paragraph></text>\n")
		buf.WriteString("      </section>\n    </component>\n")
		return
	}
	buf.WriteString("        <text><table><thead><tr><th>Medication</th><th>Status</th><th>Authored</th></tr></thead><tbody>\n")
	for _, mPtr := range meds {
		m := *mPtr
		fmt.Fprintf(buf, "          <tr><td>%s</td><td>%s</td><td>%s</td></tr>\n",
			escText(displayForCC(m["medicationCodeableConcept"])), escText(strVal(m["status"])), escText(strVal(m["authoredOn"])))
	}
	buf.WriteString("        </tbody></table></text>\n")

	for idx, mPtr := range meds {
		m := *mPtr
		idStr, _ := m["id"].(string)
		if idStr == "" {
			idStr = fmt.Sprintf("med-%d", idx+1)
		}
		cdaStatus := "active"
		switch strVal(m["status"]) {
		case "completed":
			cdaStatus = "completed"
		case "cancelled", "stopped":
			cdaStatus = "aborted"
		}
		buf.WriteString("        <entry typeCode=\"DRIV\">\n")
		buf.WriteString("          <substanceAdministration classCode=\"SBADM\" moodCode=\"EVN\">\n")
		buf.WriteString(`            <templateId root="2.16.840.1.113883.10.20.22.4.16"/>` + "\n")
		buf.WriteString(`            <templateId root="2.16.840.1.113883.10.20.22.4.16" extension="2014-06-09"/>` + "\n")
		fmt.Fprintf(buf, `            <id root="2.16.840.1.113883.19.5" extension="%s"/>`+"\n", escAttr(idStr))
		fmt.Fprintf(buf, `            <statusCode code="%s"/>`+"\n", escAttr(cdaStatus))
		writeIntervalEffectiveTime(buf, "            ", strVal(m["authoredOn"]), "")

		if doses, ok := m["dosageInstruction"].([]any); ok && len(doses) > 0 {
			if dm, ok := doses[0].(map[string]any); ok {
				if route, ok := dm["route"].(map[string]any); ok {
					writeCDACodedElement(buf, "            ", "routeCode", route, "2.16.840.1.113883.5.112")
				}
				if drs, ok := dm["doseAndRate"].([]any); ok && len(drs) > 0 {
					if dr0, ok := drs[0].(map[string]any); ok {
						if dq, ok := dr0["doseQuantity"].(map[string]any); ok {
							writeDoseQuantityEl(buf, "            ", dq)
						}
					}
				}
			}
		}

		buf.WriteString("            <consumable>\n              <manufacturedProduct classCode=\"MANU\">\n")
		buf.WriteString(`                <templateId root="2.16.840.1.113883.10.20.22.4.23"/>` + "\n")
		buf.WriteString("                <manufacturedMaterial>\n")
		writeCDACodedElement(buf, "                  ", "code", m["medicationCodeableConcept"], "2.16.840.1.113883.6.88")
		buf.WriteString("                </manufacturedMaterial>\n              </manufacturedProduct>\n            </consumable>\n")
		buf.WriteString("          </substanceAdministration>\n        </entry>\n")
	}
	buf.WriteString("      </section>\n    </component>\n")
}

func writeImmunizationsSection(buf *bytes.Buffer, immuns []*map[string]any) {
	writeSectionStart(buf, SectionImmunizations, SectionImmunEntries, "11369-6", "History of Immunization Narrative", "Immunizations", len(immuns) == 0)
	if len(immuns) == 0 {
		buf.WriteString("        <text><paragraph>No immunizations recorded.</paragraph></text>\n")
		buf.WriteString("      </section>\n    </component>\n")
		return
	}
	buf.WriteString("        <text><table><thead><tr><th>Vaccine</th><th>Status</th><th>Date</th></tr></thead><tbody>\n")
	for _, iPtr := range immuns {
		im := *iPtr
		fmt.Fprintf(buf, "          <tr><td>%s</td><td>%s</td><td>%s</td></tr>\n",
			escText(displayForCC(im["vaccineCode"])), escText(strVal(im["status"])), escText(strVal(im["occurrenceDateTime"])))
	}
	buf.WriteString("        </tbody></table></text>\n")

	for idx, iPtr := range immuns {
		im := *iPtr
		idStr, _ := im["id"].(string)
		if idStr == "" {
			idStr = fmt.Sprintf("imm-%d", idx+1)
		}
		negInd := "false"
		if strVal(im["status"]) == "not-done" {
			negInd = "true"
		}
		buf.WriteString("        <entry typeCode=\"DRIV\">\n")
		fmt.Fprintf(buf, "          <substanceAdministration classCode=\"SBADM\" moodCode=\"EVN\" negationInd=\"%s\">\n", negInd)
		buf.WriteString(`            <templateId root="2.16.840.1.113883.10.20.22.4.52"/>` + "\n")
		buf.WriteString(`            <templateId root="2.16.840.1.113883.10.20.22.4.52" extension="2015-08-01"/>` + "\n")
		fmt.Fprintf(buf, `            <id root="2.16.840.1.113883.19.5" extension="%s"/>`+"\n", escAttr(idStr))
		buf.WriteString(`            <statusCode code="completed"/>` + "\n")
		if ts := FormatFHIRToCDATimestamp(strVal(im["occurrenceDateTime"])); ts != "" {
			fmt.Fprintf(buf, `            <effectiveTime value="%s"/>`+"\n", escAttr(ts))
		} else {
			buf.WriteString(`            <effectiveTime nullFlavor="UNK"/>` + "\n")
		}
		if route, ok := im["route"].(map[string]any); ok {
			writeCDACodedElement(buf, "            ", "routeCode", route, "2.16.840.1.113883.5.112")
		}
		if dq, ok := im["doseQuantity"].(map[string]any); ok {
			writeDoseQuantityEl(buf, "            ", dq)
		}
		buf.WriteString("            <consumable>\n              <manufacturedProduct classCode=\"MANU\">\n")
		buf.WriteString(`                <templateId root="2.16.840.1.113883.10.20.22.4.54"/>` + "\n")
		buf.WriteString("                <manufacturedMaterial>\n")
		writeCDACodedElement(buf, "                  ", "code", im["vaccineCode"], "2.16.840.1.113883.12.292")
		if lot := strVal(im["lotNumber"]); lot != "" {
			fmt.Fprintf(buf, "                  <lotNumberText>%s</lotNumberText>\n", escText(lot))
		}
		buf.WriteString("                </manufacturedMaterial>\n")
		if mfg, ok := im["manufacturer"].(map[string]any); ok {
			if disp := strVal(mfg["display"]); disp != "" {
				fmt.Fprintf(buf, "                <manufacturerOrganization><name>%s</name></manufacturerOrganization>\n", escText(disp))
			}
		}
		buf.WriteString("              </manufacturedProduct>\n            </consumable>\n")
		buf.WriteString("          </substanceAdministration>\n        </entry>\n")
	}
	buf.WriteString("      </section>\n    </component>\n")
}

func writeResultsSection(buf *bytes.Buffer, reports []*map[string]any, labs []*map[string]any, obsByID map[string]map[string]any, now time.Time) {
	isEmpty := len(reports) == 0 && len(labs) == 0
	writeSectionStart(buf, SectionResults, SectionResultsEntries, "30954-2", "Relevant diagnostic tests/laboratory data Narrative", "Results", isEmpty)
	if isEmpty {
		buf.WriteString("        <text><paragraph>No laboratory results recorded.</paragraph></text>\n")
		buf.WriteString("      </section>\n    </component>\n")
		return
	}

	usedObs := make(map[string]bool)
	buf.WriteString("        <text><table><thead><tr><th>Test</th><th>Value</th><th>Date</th></tr></thead><tbody>\n")
	for _, oPtr := range labs {
		o := *oPtr
		fmt.Fprintf(buf, "          <tr><td>%s</td><td>%s</td><td>%s</td></tr>\n",
			escText(displayForCC(o["code"])), escText(obsValueSummary(o)), escText(strVal(o["effectiveDateTime"])))
	}
	buf.WriteString("        </tbody></table></text>\n")

	for idx, drPtr := range reports {
		dr := *drPtr
		drID, _ := dr["id"].(string)
		if drID == "" {
			drID = fmt.Sprintf("dr-%d", idx+1)
		}
		var childObs []map[string]any
		if resRefs, ok := dr["result"].([]any); ok {
			for _, rr := range resRefs {
				rm, ok := rr.(map[string]any)
				if !ok {
					continue
				}
				ref, _ := rm["reference"].(string)
				if om, exists := obsByID[ref]; exists {
					childObs = append(childObs, om)
					if oid, _ := om["id"].(string); oid != "" {
						usedObs[oid] = true
					}
				}
			}
		}
		buf.WriteString("        <entry typeCode=\"DRIV\">\n")
		buf.WriteString("          <organizer classCode=\"BATTERY\" moodCode=\"EVN\">\n")
		buf.WriteString(`            <templateId root="2.16.840.1.113883.10.20.22.4.1"/>` + "\n")
		buf.WriteString(`            <templateId root="2.16.840.1.113883.10.20.22.4.1" extension="2015-08-01"/>` + "\n")
		fmt.Fprintf(buf, `            <id root="2.16.840.1.113883.19.5" extension="%s"/>`+"\n", escAttr(drID))
		writeCDACodedElement(buf, "            ", "code", dr["code"], "2.16.840.1.113883.6.1")
		buf.WriteString(`            <statusCode code="completed"/>` + "\n")
		effTS := FormatFHIRToCDATimestamp(firstStr(strVal(dr["effectiveDateTime"]), strVal(dr["issued"]), now.Format(time.RFC3339)))
		fmt.Fprintf(buf, `            <effectiveTime value="%s"/>`+"\n", escAttr(effTS))
		for cIdx, co := range childObs {
			writeResultComponentObs(buf, co, fmt.Sprintf("%s-obs-%d", drID, cIdx+1))
		}
		buf.WriteString("          </organizer>\n        </entry>\n")
	}

	// Unlinked lab observations grouped into a summary battery organizer
	var unlinked []map[string]any
	for _, oPtr := range labs {
		o := *oPtr
		oid, _ := o["id"].(string)
		if oid == "" || !usedObs[oid] {
			unlinked = append(unlinked, o)
		}
	}
	if len(unlinked) > 0 {
		buf.WriteString("        <entry typeCode=\"DRIV\">\n")
		buf.WriteString("          <organizer classCode=\"BATTERY\" moodCode=\"EVN\">\n")
		buf.WriteString(`            <templateId root="2.16.840.1.113883.10.20.22.4.1"/>` + "\n")
		buf.WriteString(`            <id root="2.16.840.1.113883.19.5" extension="lab-battery-unlinked"/>` + "\n")
		buf.WriteString(`            <code code="26436-6" codeSystem="2.16.840.1.113883.6.1" displayName="Laboratory studies (set)"/>` + "\n")
		buf.WriteString(`            <statusCode code="completed"/>` + "\n")
		for idx, o := range unlinked {
			writeResultComponentObs(buf, o, fmt.Sprintf("lab-%d", idx+1))
		}
		buf.WriteString("          </organizer>\n        </entry>\n")
	}

	buf.WriteString("      </section>\n    </component>\n")
}

func writeResultComponentObs(buf *bytes.Buffer, obs map[string]any, fallbackID string) {
	oid := firstStr(strVal(obs["id"]), fallbackID)
	buf.WriteString("            <component>\n              <observation classCode=\"OBS\" moodCode=\"EVN\">\n")
	buf.WriteString(`                <templateId root="2.16.840.1.113883.10.20.22.4.2"/>` + "\n")
	buf.WriteString(`                <templateId root="2.16.840.1.113883.10.20.22.4.2" extension="2015-08-01"/>` + "\n")
	fmt.Fprintf(buf, `                <id root="2.16.840.1.113883.19.5" extension="%s"/>`+"\n", escAttr(oid))
	writeCDACodedElement(buf, "                ", "code", obs["code"], "2.16.840.1.113883.6.1")
	buf.WriteString(`                <statusCode code="completed"/>` + "\n")
	if ts := FormatFHIRToCDATimestamp(strVal(obs["effectiveDateTime"])); ts != "" {
		fmt.Fprintf(buf, `                <effectiveTime value="%s"/>`+"\n", escAttr(ts))
	} else {
		buf.WriteString(`                <effectiveTime nullFlavor="UNK"/>` + "\n")
	}
	writeObservationCDAValue(buf, "                ", obs)
	if interps, ok := obs["interpretation"].([]any); ok && len(interps) > 0 {
		writeCDACodedElement(buf, "                ", "interpretationCode", interps[0], "2.16.840.1.113883.5.83")
	}
	if rrs, ok := obs["referenceRange"].([]any); ok && len(rrs) > 0 {
		if rrm, ok := rrs[0].(map[string]any); ok {
			writeReferenceRangeEl(buf, "                ", rrm)
		}
	}
	buf.WriteString("              </observation>\n            </component>\n")
}

func writeVitalsSection(buf *bytes.Buffer, vitals []*map[string]any, now time.Time) {
	writeSectionStart(buf, SectionVitals, SectionVitalsEntries, "8716-3", "Vital signs", "Vital Signs", len(vitals) == 0)
	if len(vitals) == 0 {
		buf.WriteString("        <text><paragraph>No vital signs recorded.</paragraph></text>\n")
		buf.WriteString("      </section>\n    </component>\n")
		return
	}
	buf.WriteString("        <text><table><thead><tr><th>Vital Sign</th><th>Value</th><th>Date</th></tr></thead><tbody>\n")
	for _, vPtr := range vitals {
		v := *vPtr
		fmt.Fprintf(buf, "          <tr><td>%s</td><td>%s</td><td>%s</td></tr>\n",
			escText(displayForCC(v["code"])), escText(obsValueSummary(v)), escText(strVal(v["effectiveDateTime"])))
	}
	buf.WriteString("        </tbody></table></text>\n")

	buf.WriteString("        <entry typeCode=\"DRIV\">\n")
	buf.WriteString("          <organizer classCode=\"CLUSTER\" moodCode=\"EVN\">\n")
	buf.WriteString(`            <templateId root="2.16.840.1.113883.10.20.22.4.26"/>` + "\n")
	buf.WriteString(`            <templateId root="2.16.840.1.113883.10.20.22.4.26" extension="2015-08-01"/>` + "\n")
	buf.WriteString(`            <id root="2.16.840.1.113883.19.5" extension="vitals-panel-1"/>` + "\n")
	buf.WriteString(`            <code code="46680005" codeSystem="2.16.840.1.113883.6.96" codeSystemName="SNOMED CT" displayName="Vital signs"><translation code="74728-7" codeSystem="2.16.840.1.113883.6.1" codeSystemName="LOINC" displayName="Vital signs"/></code>` + "\n")
	buf.WriteString(`            <statusCode code="completed"/>` + "\n")
	fmt.Fprintf(buf, `            <effectiveTime value="%s"/>`+"\n", escAttr(FormatFHIRToCDATimestamp(now.Format(time.RFC3339))))

	for idx, vPtr := range vitals {
		v := *vPtr
		vid := firstStr(strVal(v["id"]), fmt.Sprintf("vital-%d", idx+1))
		// Flatten multi-component vitals (e.g., Blood Pressure 85354-9 / 96607-7) into C-CDA vital sign components
		if comps, ok := v["component"].([]any); ok && len(comps) > 0 {
			for cIdx, rawComp := range comps {
				cm, ok := rawComp.(map[string]any)
				if !ok {
					continue
				}
				flatObs := map[string]any{
					"id":                fmt.Sprintf("%s-c%d", vid, cIdx+1),
					"code":              cm["code"],
					"effectiveDateTime": v["effectiveDateTime"],
					"valueQuantity":     cm["valueQuantity"],
					"valueString":       cm["valueString"],
				}
				writeVitalComponentObs(buf, flatObs)
			}
			continue
		}
		writeVitalComponentObs(buf, v)
	}

	buf.WriteString("          </organizer>\n        </entry>\n")
	buf.WriteString("      </section>\n    </component>\n")
}

func writeVitalComponentObs(buf *bytes.Buffer, obs map[string]any) {
	oid := firstStr(strVal(obs["id"]), "vital")
	buf.WriteString("            <component>\n              <observation classCode=\"OBS\" moodCode=\"EVN\">\n")
	buf.WriteString(`                <templateId root="2.16.840.1.113883.10.20.22.4.27"/>` + "\n")
	buf.WriteString(`                <templateId root="2.16.840.1.113883.10.20.22.4.27" extension="2014-06-09"/>` + "\n")
	fmt.Fprintf(buf, `                <id root="2.16.840.1.113883.19.5" extension="%s"/>`+"\n", escAttr(oid))
	writeCDACodedElement(buf, "                ", "code", obs["code"], "2.16.840.1.113883.6.1")
	buf.WriteString(`                <statusCode code="completed"/>` + "\n")
	if ts := FormatFHIRToCDATimestamp(strVal(obs["effectiveDateTime"])); ts != "" {
		fmt.Fprintf(buf, `                <effectiveTime value="%s"/>`+"\n", escAttr(ts))
	} else {
		buf.WriteString(`                <effectiveTime nullFlavor="UNK"/>` + "\n")
	}
	writeObservationCDAValue(buf, "                ", obs)
	buf.WriteString("              </observation>\n            </component>\n")
}

func writeProceduresSection(buf *bytes.Buffer, procs []*map[string]any) {
	writeSectionStart(buf, SectionProcedures, SectionProcEntries, "47519-4", "History of Procedures Document", "Procedures", len(procs) == 0)
	if len(procs) == 0 {
		buf.WriteString("        <text><paragraph>No procedures recorded.</paragraph></text>\n")
		buf.WriteString("      </section>\n    </component>\n")
		return
	}
	buf.WriteString("        <text><table><thead><tr><th>Procedure</th><th>Date</th></tr></thead><tbody>\n")
	for _, pPtr := range procs {
		p := *pPtr
		dt := firstStr(strVal(p["performedDateTime"]), periodStart(p["performedPeriod"]))
		fmt.Fprintf(buf, "          <tr><td>%s</td><td>%s</td></tr>\n", escText(displayForCC(p["code"])), escText(dt))
	}
	buf.WriteString("        </tbody></table></text>\n")

	for idx, pPtr := range procs {
		p := *pPtr
		idStr := firstStr(strVal(p["id"]), fmt.Sprintf("proc-%d", idx+1))
		buf.WriteString("        <entry typeCode=\"DRIV\">\n")
		buf.WriteString("          <procedure classCode=\"PROC\" moodCode=\"EVN\">\n")
		buf.WriteString(`            <templateId root="2.16.840.1.113883.10.20.22.4.14"/>` + "\n")
		buf.WriteString(`            <templateId root="2.16.840.1.113883.10.20.22.4.14" extension="2014-06-09"/>` + "\n")
		fmt.Fprintf(buf, `            <id root="2.16.840.1.113883.19.5" extension="%s"/>`+"\n", escAttr(idStr))
		writeCDACodedElement(buf, "            ", "code", p["code"], "2.16.840.1.113883.6.96")
		buf.WriteString(`            <statusCode code="completed"/>` + "\n")
		if ts := FormatFHIRToCDATimestamp(firstStr(strVal(p["performedDateTime"]), periodStart(p["performedPeriod"]))); ts != "" {
			fmt.Fprintf(buf, `            <effectiveTime value="%s"/>`+"\n", escAttr(ts))
		}
		if sites, ok := p["bodySite"].([]any); ok && len(sites) > 0 {
			writeCDACodedElement(buf, "            ", "targetSiteCode", sites[0], "2.16.840.1.113883.6.96")
		}
		buf.WriteString("          </procedure>\n        </entry>\n")
	}
	buf.WriteString("      </section>\n    </component>\n")
}

func writeEncountersSection(buf *bytes.Buffer, encs []*map[string]any) {
	writeSectionStart(buf, SectionEncounters, SectionEncEntries, "46240-8", "History of hospitalizations+outpatient visits Narrative", "Encounters", len(encs) == 0)
	if len(encs) == 0 {
		buf.WriteString("        <text><paragraph>No encounters recorded.</paragraph></text>\n")
		buf.WriteString("      </section>\n    </component>\n")
		return
	}
	buf.WriteString("        <text><table><thead><tr><th>Encounter</th><th>Start</th><th>End</th></tr></thead><tbody>\n")
	for _, ePtr := range encs {
		e := *ePtr
		fmt.Fprintf(buf, "          <tr><td>%s</td><td>%s</td><td>%s</td></tr>\n",
			escText(firstEncounterDisplay(e)), escText(periodStart(e["period"])), escText(periodEnd(e["period"])))
	}
	buf.WriteString("        </tbody></table></text>\n")

	for idx, ePtr := range encs {
		e := *ePtr
		idStr := firstStr(strVal(e["id"]), fmt.Sprintf("enc-%d", idx+1))
		buf.WriteString("        <entry typeCode=\"DRIV\">\n")
		buf.WriteString("          <encounter classCode=\"ENC\" moodCode=\"EVN\">\n")
		buf.WriteString(`            <templateId root="2.16.840.1.113883.10.20.22.4.49"/>` + "\n")
		buf.WriteString(`            <templateId root="2.16.840.1.113883.10.20.22.4.49" extension="2015-08-01"/>` + "\n")
		fmt.Fprintf(buf, `            <id root="2.16.840.1.113883.19.5" extension="%s"/>`+"\n", escAttr(idStr))
		if types, ok := e["type"].([]any); ok && len(types) > 0 {
			writeCDACodedElement(buf, "            ", "code", types[0], "2.16.840.1.113883.6.12")
		} else {
			buf.WriteString(`            <code code="99213" codeSystem="2.16.840.1.113883.6.12" displayName="Office outpatient visit"/>` + "\n")
		}
		writeIntervalEffectiveTime(buf, "            ", periodStart(e["period"]), periodEnd(e["period"]))
		buf.WriteString("          </encounter>\n        </entry>\n")
	}
	buf.WriteString("      </section>\n    </component>\n")
}

func writeSectionStart(buf *bytes.Buffer, baseTpl, entryTpl, loincCode, loincDisp, title string, isEmpty bool) {
	if isEmpty {
		buf.WriteString("    <component>\n      <section nullFlavor=\"NI\">\n")
	} else {
		buf.WriteString("    <component>\n      <section>\n")
	}
	fmt.Fprintf(buf, `        <templateId root="%s"/>`+"\n", baseTpl)
	fmt.Fprintf(buf, `        <templateId root="%s"/>`+"\n", entryTpl)
	fmt.Fprintf(buf, `        <templateId root="%s" extension="2015-08-01"/>`+"\n", entryTpl)
	fmt.Fprintf(buf, `        <code code="%s" codeSystem="2.16.840.1.113883.6.1" codeSystemName="LOINC" displayName="%s"/>`+"\n", loincCode, escAttr(loincDisp))
	fmt.Fprintf(buf, `        <title>%s</title>`+"\n", escText(title))
}

func writeCDACodedElement(buf *bytes.Buffer, indent, tag string, raw any, defaultOID string) {
	writeCodedElementInternal(buf, indent, tag, "", raw, defaultOID)
}

func writeCDACodedValue(buf *bytes.Buffer, indent string, raw any, defaultOID string) {
	writeCodedElementInternal(buf, indent, "value", "CD", raw, defaultOID)
}

func writeCodedElementInternal(buf *bytes.Buffer, indent, tag, xsiType string, raw any, defaultOID string) {
	m, ok := raw.(map[string]any)
	if !ok || m == nil {
		fmt.Fprintf(buf, `%s<%s nullFlavor="UNK"/>`+"\n", indent, tag)
		return
	}
	codings, _ := m["coding"].([]any)
	if len(codings) == 0 {
		if xsiType != "" {
			fmt.Fprintf(buf, `%s<%s xsi:type="%s" nullFlavor="OTH"><originalText>%s</originalText></%s>`+"\n",
				indent, tag, xsiType, escText(strVal(m["text"])), tag)
		} else {
			fmt.Fprintf(buf, `%s<%s nullFlavor="OTH"><originalText>%s</originalText></%s>`+"\n",
				indent, tag, escText(strVal(m["text"])), tag)
		}
		return
	}
	c0, _ := codings[0].(map[string]any)
	code := strVal(c0["code"])
	sysURI := strVal(c0["system"])
	disp := strVal(c0["display"])
	oid, sysName := FHIRSystemToOID(sysURI)
	if oid == "" {
		oid = defaultOID
	}

	fmt.Fprintf(buf, "%s<%s", indent, tag)
	if xsiType != "" {
		fmt.Fprintf(buf, ` xsi:type="%s"`, xsiType)
	}
	if code != "" {
		fmt.Fprintf(buf, ` code="%s"`, escAttr(code))
	}
	if oid != "" {
		fmt.Fprintf(buf, ` codeSystem="%s"`, escAttr(oid))
	}
	if sysName != "" {
		fmt.Fprintf(buf, ` codeSystemName="%s"`, escAttr(sysName))
	}
	if disp != "" {
		fmt.Fprintf(buf, ` displayName="%s"`, escAttr(disp))
	}
	if len(codings) == 1 {
		buf.WriteString("/>\n")
		return
	}
	buf.WriteString(">\n")
	for _, trRaw := range codings[1:] {
		tr, ok := trRaw.(map[string]any)
		if !ok {
			continue
		}
		tCode := strVal(tr["code"])
		tOID, tName := FHIRSystemToOID(strVal(tr["system"]))
		tDisp := strVal(tr["display"])
		if tCode == "" {
			continue
		}
		fmt.Fprintf(buf, `%s  <translation code="%s" codeSystem="%s"`, indent, escAttr(tCode), escAttr(firstStr(tOID, defaultOID)))
		if tName != "" {
			fmt.Fprintf(buf, ` codeSystemName="%s"`, escAttr(tName))
		}
		if tDisp != "" {
			fmt.Fprintf(buf, ` displayName="%s"`, escAttr(tDisp))
		}
		buf.WriteString("/>\n")
	}
	fmt.Fprintf(buf, "%s</%s>\n", indent, tag)
}

func writeObservationCDAValue(buf *bytes.Buffer, indent string, obs map[string]any) {
	if vq, ok := obs["valueQuantity"].(map[string]any); ok {
		valStr := numString(vq["value"])
		unitStr := firstStr(strVal(vq["code"]), strVal(vq["unit"]), "1")
		if valStr != "" {
			fmt.Fprintf(buf, `%s<value xsi:type="PQ" value="%s" unit="%s"/>`+"\n", indent, escAttr(valStr), escAttr(unitStr))
			return
		}
	}
	if vcc, ok := obs["valueCodeableConcept"].(map[string]any); ok {
		writeCDACodedValue(buf, indent, vcc, "2.16.840.1.113883.6.96")
		return
	}
	if vs, ok := obs["valueString"].(string); ok && vs != "" {
		fmt.Fprintf(buf, `%s<value xsi:type="ST">%s</value>`+"\n", indent, escText(vs))
		return
	}
	fmt.Fprintf(buf, `%s<value xsi:type="ST" nullFlavor="UNK"/>`+"\n", indent)
}

func writeDoseQuantityEl(buf *bytes.Buffer, indent string, dq map[string]any) {
	valStr := numString(dq["value"])
	if valStr == "" {
		return
	}
	unit := firstStr(strVal(dq["code"]), strVal(dq["unit"]))
	if unit != "" {
		fmt.Fprintf(buf, `%s<doseQuantity value="%s" unit="%s"/>`+"\n", indent, escAttr(valStr), escAttr(unit))
		return
	}
	fmt.Fprintf(buf, `%s<doseQuantity value="%s"/>`+"\n", indent, escAttr(valStr))
}

func writeReferenceRangeEl(buf *bytes.Buffer, indent string, rrm map[string]any) {
	buf.WriteString(indent + "<referenceRange>\n" + indent + "  <observationRange>\n")
	if txt := strVal(rrm["text"]); txt != "" {
		fmt.Fprintf(buf, "%s    <text>%s</text>\n", indent, escText(txt))
	}
	low, _ := rrm["low"].(map[string]any)
	high, _ := rrm["high"].(map[string]any)
	if low != nil || high != nil {
		buf.WriteString(indent + `    <value xsi:type="IVL_PQ">` + "\n")
		if low != nil {
			if lVal := numString(low["value"]); lVal != "" {
				lUnit := firstStr(strVal(low["code"]), strVal(low["unit"]), "1")
				fmt.Fprintf(buf, `%s      <low value="%s" unit="%s"/>`+"\n", indent, escAttr(lVal), escAttr(lUnit))
			}
		}
		if high != nil {
			if hVal := numString(high["value"]); hVal != "" {
				hUnit := firstStr(strVal(high["code"]), strVal(high["unit"]), "1")
				fmt.Fprintf(buf, `%s      <high value="%s" unit="%s"/>`+"\n", indent, escAttr(hVal), escAttr(hUnit))
			}
		}
		buf.WriteString(indent + "    </value>\n")
	}
	buf.WriteString(indent + "  </observationRange>\n" + indent + "</referenceRange>\n")
}

func writeIntervalEffectiveTime(buf *bytes.Buffer, indent, startRaw, endRaw string) {
	sTS := FormatFHIRToCDATimestamp(startRaw)
	eTS := FormatFHIRToCDATimestamp(endRaw)
	if sTS == "" && eTS == "" {
		fmt.Fprintf(buf, `%s<effectiveTime nullFlavor="UNK"/>`+"\n", indent)
		return
	}
	fmt.Fprintf(buf, "%s<effectiveTime>\n", indent)
	if sTS != "" {
		fmt.Fprintf(buf, `%s  <low value="%s"/>`+"\n", indent, escAttr(sTS))
	} else {
		fmt.Fprintf(buf, `%s  <low nullFlavor="UNK"/>`+"\n", indent)
	}
	if eTS != "" {
		fmt.Fprintf(buf, `%s  <high value="%s"/>`+"\n", indent, escAttr(eTS))
	}
	fmt.Fprintf(buf, "%s</effectiveTime>\n", indent)
}

func writeCDAName(buf *bytes.Buffer, indent string, nm map[string]any) {
	useAttr := ""
	if strVal(nm["use"]) == "official" {
		useAttr = ` use="L"`
	}
	fmt.Fprintf(buf, "%s<name%s>\n", indent, useAttr)
	if givens, ok := nm["given"].([]any); ok {
		for _, g := range givens {
			if gs := strVal(g); gs != "" {
				fmt.Fprintf(buf, "%s  <given>%s</given>\n", indent, escText(gs))
			}
		}
	}
	if fam := strVal(nm["family"]); fam != "" {
		fmt.Fprintf(buf, "%s  <family>%s</family>\n", indent, escText(fam))
	}
	if prefs, ok := nm["prefix"].([]any); ok {
		for _, p := range prefs {
			if ps := strVal(p); ps != "" {
				fmt.Fprintf(buf, "%s  <prefix>%s</prefix>\n", indent, escText(ps))
			}
		}
	}
	if suffs, ok := nm["suffix"].([]any); ok {
		for _, s := range suffs {
			if ss := strVal(s); ss != "" {
				fmt.Fprintf(buf, "%s  <suffix>%s</suffix>\n", indent, escText(ss))
			}
		}
	}
	fmt.Fprintf(buf, "%s</name>\n", indent)
}

func writeCDAAd(buf *bytes.Buffer, indent string, am map[string]any) {
	useAttr := ` use="HP"`
	if strVal(am["use"]) == "work" {
		useAttr = ` use="WP"`
	}
	fmt.Fprintf(buf, "%s<addr%s>\n", indent, useAttr)
	if lines, ok := am["line"].([]any); ok {
		for _, l := range lines {
			if ls := strVal(l); ls != "" {
				fmt.Fprintf(buf, "%s  <streetAddressLine>%s</streetAddressLine>\n", indent, escText(ls))
			}
		}
	}
	for _, pair := range [][2]string{{"city", "city"}, {"state", "state"}, {"postalCode", "postalCode"}, {"country", "country"}} {
		if v := strVal(am[pair[0]]); v != "" {
			fmt.Fprintf(buf, "%s  <%s>%s</%s>\n", indent, pair[1], escText(v), pair[1])
		}
	}
	fmt.Fprintf(buf, "%s</addr>\n", indent)
}

func writeCDATel(buf *bytes.Buffer, indent string, tm map[string]any) {
	val := strVal(tm["value"])
	if val == "" {
		return
	}
	sys := strings.ToLower(strVal(tm["system"]))
	prefix := "tel:"
	if sys == "email" {
		prefix = "mailto:"
	}
	useCode := "HP"
	if strVal(tm["use"]) == "work" {
		useCode = "WP"
	} else if strVal(tm["use"]) == "mobile" {
		useCode = "MC"
	}
	fmt.Fprintf(buf, `%s<telecom value="%s%s" use="%s"/>`+"\n", indent, prefix, escAttr(val), useCode)
}

func isVitalObservation(obs map[string]any) bool {
	if cats, ok := obs["category"].([]any); ok {
		for _, c := range cats {
			if firstCodingCode(c) == "vital-signs" {
				return true
			}
		}
	}
	return vitalSignLOINCs[firstCodingCode(obs["code"])]
}

func fhirGenderToCDA(g string) (code, display string) {
	switch strings.ToLower(strings.TrimSpace(g)) {
	case "male":
		return "M", "Male"
	case "female":
		return "F", "Female"
	default:
		return "UN", "Undifferentiated"
	}
}

func firstCodingCode(raw any) string {
	m, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	codings, _ := m["coding"].([]any)
	if len(codings) == 0 {
		return ""
	}
	c0, _ := codings[0].(map[string]any)
	return strVal(c0["code"])
}

func displayForCC(raw any) string {
	m, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	if txt := strVal(m["text"]); txt != "" {
		return txt
	}
	codings, _ := m["coding"].([]any)
	if len(codings) > 0 {
		if c0, ok := codings[0].(map[string]any); ok {
			return firstStr(strVal(c0["display"]), strVal(c0["code"]))
		}
	}
	return ""
}

func obsValueSummary(obs map[string]any) string {
	if vq, ok := obs["valueQuantity"].(map[string]any); ok {
		return strings.TrimSpace(numString(vq["value"]) + " " + strVal(vq["unit"]))
	}
	if vcc, ok := obs["valueCodeableConcept"].(map[string]any); ok {
		return displayForCC(vcc)
	}
	return strVal(obs["valueString"])
}

func firstReactionDisplay(al map[string]any) string {
	rxns, _ := al["reaction"].([]any)
	if len(rxns) == 0 {
		return ""
	}
	r0, _ := rxns[0].(map[string]any)
	manifs, _ := r0["manifestation"].([]any)
	if len(manifs) == 0 {
		return ""
	}
	return displayForCC(manifs[0])
}

func firstEncounterDisplay(enc map[string]any) string {
	if types, ok := enc["type"].([]any); ok && len(types) > 0 {
		return displayForCC(types[0])
	}
	if cls, ok := enc["class"].(map[string]any); ok {
		return firstStr(strVal(cls["display"]), strVal(cls["code"]))
	}
	return "Encounter"
}

func periodStart(raw any) string {
	if p, ok := raw.(map[string]any); ok {
		return strVal(p["start"])
	}
	return ""
}

func periodEnd(raw any) string {
	if p, ok := raw.(map[string]any); ok {
		return strVal(p["end"])
	}
	return ""
}

func numString(v any) string {
	switch tv := v.(type) {
	case float64:
		return strconv.FormatFloat(tv, 'f', -1, 64)
	case int:
		return strconv.Itoa(tv)
	case int64:
		return strconv.FormatInt(tv, 10)
	case string:
		return strings.TrimSpace(tv)
	default:
		if v == nil {
			return ""
		}
		return strings.TrimSpace(fmt.Sprintf("%v", v))
	}
}

func strVal(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func escText(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

func escAttr(s string) string {
	return escText(s)
}
