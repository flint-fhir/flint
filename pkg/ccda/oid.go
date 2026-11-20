// Package ccda implements a pure-Go, XXE-hardened HL7 C-CDA R2.1 Continuity of Care
// Document (CCD) parser and bidirectional FHIR R4 <-> C-CDA R2.1 converter covering
// the Patient header and 8 core clinical sections (Problems, Allergies, Medications,
// Immunizations, Results, Vital Signs, Procedures, and Encounters).
package ccda

import (
	"fmt"
	"strings"
	"time"
)

type oidMapping struct {
	oid        string
	fhirSystem string
	name       string
}

var knownOIDMappings = []oidMapping{
	{oid: "2.16.840.1.113883.6.96", fhirSystem: "http://snomed.info/sct", name: "SNOMED CT"},
	{oid: "2.16.840.1.113883.6.1", fhirSystem: "http://loinc.org", name: "LOINC"},
	{oid: "2.16.840.1.113883.6.88", fhirSystem: "http://www.nlm.nih.gov/research/umls/rxnorm", name: "RxNorm"},
	{oid: "2.16.840.1.113883.12.292", fhirSystem: "http://hl7.org/fhir/sid/cvx", name: "CVX"},
	{oid: "2.16.840.1.113883.6.90", fhirSystem: "http://hl7.org/fhir/sid/icd-10-cm", name: "ICD-10-CM"},
	{oid: "2.16.840.1.113883.6.103", fhirSystem: "http://hl7.org/fhir/sid/icd-9-cm", name: "ICD-9-CM"},
	{oid: "2.16.840.1.113883.6.12", fhirSystem: "http://www.ama-assn.org/go/cpt", name: "CPT"},
	{oid: "2.16.840.1.113883.6.69", fhirSystem: "http://hl7.org/fhir/sid/ndc", name: "NDC"},
	{oid: "2.16.840.1.113883.12.227", fhirSystem: "http://hl7.org/fhir/sid/mvx", name: "MVX"},
	{oid: "2.16.840.1.113883.6.8", fhirSystem: "http://unitsofmeasure.org", name: "UCUM"},
	{oid: "2.16.840.1.113883.4.6", fhirSystem: "http://hl7.org/fhir/sid/us-npi", name: "NPI"},
	{oid: "2.16.840.1.113883.4.1", fhirSystem: "http://hl7.org/fhir/sid/us-ssn", name: "SSN"},
	{oid: "2.16.840.1.113883.6.238", fhirSystem: "urn:oid:2.16.840.1.113883.6.238", name: "CDC Race and Ethnicity"},
	{oid: "2.16.840.1.113883.5.1", fhirSystem: "http://terminology.hl7.org/CodeSystem/v3-AdministrativeGender", name: "AdministrativeGender"},
	{oid: "2.16.840.1.113883.5.2", fhirSystem: "http://terminology.hl7.org/CodeSystem/v3-MaritalStatus", name: "MaritalStatus"},
	{oid: "2.16.840.1.113883.5.4", fhirSystem: "http://terminology.hl7.org/CodeSystem/v3-ActCode", name: "ActCode"},
	{oid: "2.16.840.1.113883.5.83", fhirSystem: "http://terminology.hl7.org/CodeSystem/v3-ObservationInterpretation", name: "ObservationInterpretation"},
	{oid: "2.16.840.1.113883.5.112", fhirSystem: "http://terminology.hl7.org/CodeSystem/v3-RouteOfAdministration", name: "RouteOfAdministration"},
	{oid: "2.16.840.1.113883.3.26.1.1", fhirSystem: "http://ncicb.nci.nih.gov/xml/owl/EVS/Thesaurus.owl", name: "NCI Thesaurus"},
}

// OIDToFHIRSystem translates an HL7 v3 OID (or urn:oid:...) into its canonical FHIR R4 system URI.
func OIDToFHIRSystem(oid string) string {
	s := strings.TrimSpace(oid)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return s
	}
	trimmedOID := strings.TrimPrefix(s, "urn:oid:")
	for _, m := range knownOIDMappings {
		if m.oid == trimmedOID {
			return m.fhirSystem
		}
	}
	if strings.HasPrefix(s, "urn:") {
		return s
	}
	if isOID(trimmedOID) {
		return "urn:oid:" + trimmedOID
	}
	return "urn:flint:ccda:system:" + sanitizeSlug(s)
}

// FHIRSystemToOID translates a canonical FHIR R4 terminology/identifier system URI into an HL7 v3 OID and display name.
func FHIRSystemToOID(sys string) (oid string, name string) {
	s := strings.TrimSpace(sys)
	if s == "" {
		return "", ""
	}
	if strings.HasPrefix(s, "urn:oid:") {
		stripped := strings.TrimPrefix(s, "urn:oid:")
		for _, m := range knownOIDMappings {
			if m.oid == stripped {
				return m.oid, m.name
			}
		}
		return stripped, ""
	}
	for _, m := range knownOIDMappings {
		if m.fhirSystem == s {
			return m.oid, m.name
		}
	}
	if isOID(s) {
		return s, ""
	}
	return s, ""
}

// NullFlavorToDataAbsentReason maps an HL7 v3 NullFlavor code to a FHIR R4 data-absent-reason code.
func NullFlavorToDataAbsentReason(nf string) string {
	switch strings.ToUpper(strings.TrimSpace(nf)) {
	case "NI", "UNK", "NAV", "QS", "TRC":
		return "unknown"
	case "NA":
		return "not-applicable"
	case "ASKU":
		return "asked-unknown"
	case "NASK":
		return "not-asked"
	case "MSK":
		return "masked"
	case "OTH", "UNC", "PINF", "NINF":
		return "unsupported"
	default:
		if strings.TrimSpace(nf) == "" {
			return ""
		}
		return "unknown"
	}
}

// ParseCDATimestamp converts an HL7 v3 TS string (YYYY[MM[DD[HH[MM[SS[.SSSS]]]]]][+/-ZZZZ])
// into an ISO-8601 FHIR R4 date (when dateOnly is true or len <= 8) or dateTime.
func ParseCDATimestamp(raw string, dateOnly bool) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	// If already ISO-8601 formatted (YYYY-MM-DD...), normalize cleanly
	if len(s) >= 10 && s[4] == '-' && s[7] == '-' {
		if dateOnly {
			return s[:10], true
		}
		if len(s) == 10 {
			return s, true
		}
		if _, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return s, true
		}
	}

	var tzSuffix string
	if strings.HasSuffix(s, "Z") || strings.HasSuffix(s, "z") {
		tzSuffix = "Z"
		s = s[:len(s)-1]
	} else if plusIdx := strings.IndexAny(s, "+-"); plusIdx >= 4 {
		tzRaw := s[plusIdx:]
		s = s[:plusIdx]
		if len(tzRaw) == 5 && isDigitsOnly(tzRaw[1:]) {
			tzSuffix = tzRaw[:3] + ":" + tzRaw[3:]
		} else if len(tzRaw) == 3 && isDigitsOnly(tzRaw[1:]) {
			tzSuffix = tzRaw + ":00"
		} else {
			return "", false
		}
	}

	var frac string
	if dotIdx := strings.IndexByte(s, '.'); dotIdx >= 0 {
		fracPart := s[dotIdx+1:]
		if fracPart == "" || !isDigitsOnly(fracPart) {
			return "", false
		}
		if len(fracPart) > 4 {
			fracPart = fracPart[:4]
		}
		frac = "." + fracPart
		s = s[:dotIdx]
	}

	if !isDigitsOnly(s) || len(s) < 4 {
		return "", false
	}
	year := s[:4]
	if len(s) == 4 {
		return year, true
	}
	if len(s) < 6 {
		return "", false
	}
	month := s[4:6]
	if m := atoi2(month); m < 1 || m > 12 {
		return "", false
	}
	if len(s) == 6 {
		return year + "-" + month, true
	}
	if len(s) < 8 {
		return "", false
	}
	day := s[6:8]
	if d := atoi2(day); d < 1 || d > 31 {
		return "", false
	}
	dateStr := year + "-" + month + "-" + day
	if _, err := time.Parse("2006-01-02", dateStr); err != nil {
		return "", false
	}
	if dateOnly || len(s) == 8 {
		return dateStr, true
	}

	if len(s) != 10 && len(s) != 12 && len(s) != 14 {
		return "", false
	}
	hour := s[8:10]
	if atoi2(hour) > 23 {
		return "", false
	}
	minStr := "00"
	if len(s) >= 12 {
		minStr = s[10:12]
		if atoi2(minStr) > 59 {
			return "", false
		}
	}
	secStr := "00"
	if len(s) == 14 {
		secStr = s[12:14]
		if atoi2(secStr) > 60 {
			return "", false
		}
	}
	if tzSuffix == "" {
		tzSuffix = "Z"
	}
	dt := fmt.Sprintf("%sT%s:%s:%s%s%s", dateStr, hour, minStr, secStr, frac, tzSuffix)
	if _, err := time.Parse(time.RFC3339Nano, dt); err != nil {
		return "", false
	}
	return dt, true
}

// FormatCDAInstant returns an RFC3339 instant suitable for FHIR Composition.date,
// DocumentReference.date, DiagnosticReport.issued, and Provenance.recorded.
func FormatCDAInstant(raw string, fallback time.Time) string {
	if dt, ok := ParseCDATimestamp(raw, false); ok {
		switch len(dt) {
		case 4:
			return dt + "-01-01T00:00:00Z"
		case 7:
			return dt + "-01T00:00:00Z"
		case 10:
			return dt + "T00:00:00Z"
		default:
			return dt
		}
	}
	if fallback.IsZero() {
		fallback = time.Now().UTC()
	}
	return fallback.UTC().Format(time.RFC3339)
}

// FormatFHIRToCDATimestamp converts an ISO-8601 FHIR date/dateTime/instant into an HL7 v3 TS string.
func FormatFHIRToCDATimestamp(fhirTime string) string {
	s := strings.TrimSpace(fhirTime)
	if s == "" {
		return ""
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			_, offsetSec := t.Zone()
			sign := "+"
			if offsetSec < 0 {
				sign = "-"
				offsetSec = -offsetSec
			}
			oh := offsetSec / 3600
			om := (offsetSec % 3600) / 60
			return fmt.Sprintf("%s%s%02d%02d", t.Format("20060102150405"), sign, oh, om)
		}
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			if layout == "2006-01-02" {
				return t.Format("20060102")
			}
			return t.Format("20060102150405")
		}
	}
	if len(s) == 7 && s[4] == '-' {
		return s[:4] + s[5:7]
	}
	if len(s) == 4 && isDigitsOnly(s) {
		return s
	}
	return ""
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

func isDigitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func atoi2(s string) int {
	return int(s[0]-'0')*10 + int(s[1]-'0')
}

func sanitizeSlug(s string) string {
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
		return "unknown"
	}
	return slug
}
