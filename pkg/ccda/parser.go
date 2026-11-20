package ccda

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	// NamespaceHL7V3 is the canonical XML namespace for HL7 CDA/C-CDA R2.1 documents.
	NamespaceHL7V3 = "urn:hl7-org:v3"
	// MaxDocumentBytes bounds accepted C-CDA XML document size (16 MiB).
	MaxDocumentBytes = 16 * 1024 * 1024
	// MaxElementDepth bounds nested XML element depth to defend against XML bombs.
	MaxElementDepth = 64
)

var (
	// ErrEmptyDocument indicates an empty or whitespace-only C-CDA XML input.
	ErrEmptyDocument = errors.New("ccda: empty C-CDA XML document")
	// ErrDocumentTooLarge indicates a C-CDA payload exceeding MaxDocumentBytes.
	ErrDocumentTooLarge = errors.New("ccda: document exceeds maximum size limit")
	// ErrXXEForbidden indicates forbidden DTD / DOCTYPE / ENTITY declarations in XML input.
	ErrXXEForbidden = errors.New("ccda: forbidden DOCTYPE or ENTITY declaration")
	// ErrNestingTooDeep indicates XML nesting depth exceeding MaxElementDepth.
	ErrNestingTooDeep = errors.New("ccda: XML nesting depth exceeds limit")
	// ErrInvalidCCDA indicates malformed XML or non-ClinicalDocument root/namespace.
	ErrInvalidCCDA = errors.New("ccda: invalid C-CDA ClinicalDocument")
)

// ValidateXMLSecurity enforces strict XXE, DOCTYPE/ENTITY, nesting-depth, and
// ClinicalDocument root/namespace invariants before structural decoding.
func ValidateXMLSecurity(raw []byte) error {
	if len(raw) > MaxDocumentBytes {
		return ErrDocumentTooLarge
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ErrEmptyDocument
	}
	lower := strings.ToLower(string(trimmed))
	if strings.Contains(lower, "<!doctype") || strings.Contains(lower, "<!entity") || strings.Contains(lower, "<![include[") {
		return ErrXXEForbidden
	}

	dec := xml.NewDecoder(bytes.NewReader(trimmed))
	dec.Strict = true
	dec.Entity = map[string]string{}

	depth := 0
	rootCount := 0
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidCCDA, err)
		}
		switch t := tok.(type) {
		case xml.Directive:
			upperDir := strings.ToUpper(string(t))
			if strings.Contains(upperDir, "DOCTYPE") || strings.Contains(upperDir, "ENTITY") {
				return ErrXXEForbidden
			}
		case xml.StartElement:
			if depth == 0 {
				rootCount++
				if rootCount > 1 {
					return fmt.Errorf("%w: unexpected second root element <%s>", ErrInvalidCCDA, t.Name.Local)
				}
				if t.Name.Local != "ClinicalDocument" {
					return fmt.Errorf("%w: root element must be ClinicalDocument, got <%s>", ErrInvalidCCDA, t.Name.Local)
				}
				if t.Name.Space != "" && t.Name.Space != NamespaceHL7V3 {
					return fmt.Errorf("%w: unexpected XML namespace %q (expected %q)", ErrInvalidCCDA, t.Name.Space, NamespaceHL7V3)
				}
			}
			depth++
			if depth > MaxElementDepth {
				return ErrNestingTooDeep
			}
		case xml.EndElement:
			if depth > 0 {
				depth--
			}
		}
	}
	if rootCount == 0 {
		return ErrEmptyDocument
	}
	return nil
}

// Parse validates and unmarshals an HL7 C-CDA R2.1 ClinicalDocument XML document.
func Parse(raw []byte) (*ClinicalDocument, error) {
	if err := ValidateXMLSecurity(raw); err != nil {
		return nil, err
	}
	dec := xml.NewDecoder(bytes.NewReader(bytes.TrimSpace(raw)))
	dec.Strict = true
	dec.Entity = map[string]string{}

	var doc ClinicalDocument
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCCDA, err)
	}
	if doc.XMLName.Local != "ClinicalDocument" {
		return nil, fmt.Errorf("%w: expected ClinicalDocument root", ErrInvalidCCDA)
	}
	if doc.XMLName.Space != "" && doc.XMLName.Space != NamespaceHL7V3 {
		return nil, fmt.Errorf("%w: unexpected XML namespace %q", ErrInvalidCCDA, doc.XMLName.Space)
	}
	return &doc, nil
}

// ClinicalDocument models the HL7 C-CDA R2.1 ClinicalDocument XML schema.
type ClinicalDocument struct {
	XMLName             xml.Name       `xml:"ClinicalDocument"`
	RealmCode           *CS            `xml:"realmCode"`
	TypeID              *II            `xml:"typeId"`
	TemplateIDs         []II           `xml:"templateId"`
	ID                  II             `xml:"id"`
	Code                CD             `xml:"code"`
	Title               string         `xml:"title"`
	EffectiveTime       TS             `xml:"effectiveTime"`
	ConfidentialityCode CD             `xml:"confidentialityCode"`
	LanguageCode        CS             `xml:"languageCode"`
	SetID               *II            `xml:"setId"`
	VersionNumber       *IntAttr       `xml:"versionNumber"`
	RecordTargets       []RecordTarget `xml:"recordTarget"`
	Authors             []Author       `xml:"author"`
	Custodian           *Custodian     `xml:"custodian"`
	ComponentOf         *ComponentOf   `xml:"componentOf"`
	Component           *DocComponent  `xml:"component"`
}

type II struct {
	Root                   string `xml:"root,attr"`
	Extension              string `xml:"extension,attr"`
	NullFlavor             string `xml:"nullFlavor,attr"`
	AssigningAuthorityName string `xml:"assigningAuthorityName,attr"`
}

type CS struct {
	Code       string `xml:"code,attr"`
	NullFlavor string `xml:"nullFlavor,attr"`
}

type IntAttr struct {
	Value      string `xml:"value,attr"`
	NullFlavor string `xml:"nullFlavor,attr"`
}

type CD struct {
	Code           string    `xml:"code,attr"`
	CodeSystem     string    `xml:"codeSystem,attr"`
	CodeSystemName string    `xml:"codeSystemName,attr"`
	DisplayName    string    `xml:"displayName,attr"`
	NullFlavor     string    `xml:"nullFlavor,attr"`
	XSIType        string    `xml:"http://www.w3.org/2001/XMLSchema-instance type,attr"`
	Value          string    `xml:"value,attr"`
	Unit           string    `xml:"unit,attr"`
	OriginalText   *CDAText  `xml:"originalText"`
	Translations   []CD      `xml:"translation"`
	Low            *CDABound `xml:"low"`
	High           *CDABound `xml:"high"`
	CharText       string    `xml:",chardata"`
}

type TS struct {
	Value      string    `xml:"value,attr"`
	NullFlavor string    `xml:"nullFlavor,attr"`
	Low        *CDABound `xml:"low"`
	High       *CDABound `xml:"high"`
	Width      *CDABound `xml:"width"`
	Period     *CDABound `xml:"period"`
}

type CDABound struct {
	Value      string `xml:"value,attr"`
	Unit       string `xml:"unit,attr"`
	NullFlavor string `xml:"nullFlavor,attr"`
}

type RefAttr struct {
	Value string `xml:"value,attr"`
}

// CDAText stores both raw inner XML and extracted plain text / optional <reference value="#..."/>.
type CDAText struct {
	InnerXML  string
	PlainText string
	RefValue  string
}

// UnmarshalXML safely decodes <text> and <originalText> preserving raw XML and extracting readable text.
func (t *CDAText) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	var rawBuf struct {
		Inner string `xml:",innerxml"`
	}
	if err := d.DecodeElement(&rawBuf, &start); err != nil {
		return err
	}
	t.InnerXML = rawBuf.Inner
	t.PlainText, t.RefValue = extractTextAndRef(rawBuf.Inner)
	return nil
}

func extractTextAndRef(inner string) (plain string, refVal string) {
	if strings.TrimSpace(inner) == "" {
		return "", ""
	}
	dec := xml.NewDecoder(strings.NewReader("<wrap>" + inner + "</wrap>"))
	dec.Entity = map[string]string{}
	var sb strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch v := tok.(type) {
		case xml.StartElement:
			if v.Name.Local == "reference" {
				for _, attr := range v.Attr {
					if attr.Name.Local == "value" && refVal == "" {
						refVal = strings.TrimSpace(attr.Value)
					}
				}
			}
		case xml.CharData:
			s := strings.TrimSpace(string(v))
			if s != "" {
				if sb.Len() > 0 {
					sb.WriteByte(' ')
				}
				sb.WriteString(s)
			}
		}
	}
	return sb.String(), refVal
}

type EN struct {
	Use      string   `xml:"use,attr"`
	Families []string `xml:"family"`
	Givens   []string `xml:"given"`
	Prefixes []string `xml:"prefix"`
	Suffixes []string `xml:"suffix"`
	CharText string   `xml:",chardata"`
}

type AD struct {
	Use               string   `xml:"use,attr"`
	StreetAddressLine []string `xml:"streetAddressLine"`
	City              string   `xml:"city"`
	State             string   `xml:"state"`
	PostalCode        string   `xml:"postalCode"`
	Country           string   `xml:"country"`
}

type TEL struct {
	Value      string `xml:"value,attr"`
	Use        string `xml:"use,attr"`
	NullFlavor string `xml:"nullFlavor,attr"`
}

type RecordTarget struct {
	PatientRole PatientRole `xml:"patientRole"`
}

type PatientRole struct {
	IDs                  []II          `xml:"id"`
	Addrs                []AD          `xml:"addr"`
	Telecoms             []TEL         `xml:"telecom"`
	Patient              *CDAPatient   `xml:"patient"`
	ProviderOrganization *Organization `xml:"providerOrganization"`
}

type CDAPatient struct {
	Names                  []EN                    `xml:"name"`
	AdministrativeGender   *CD                     `xml:"administrativeGenderCode"`
	BirthTime              *TS                     `xml:"birthTime"`
	MaritalStatus          *CD                     `xml:"maritalStatusCode"`
	RaceCode               *CD                     `xml:"raceCode"`
	SDTCRaceCodes          []CD                    `xml:"urn:hl7-org:sdtc raceCode"`
	EthnicGroupCode        *CD                     `xml:"ethnicGroupCode"`
	SDTCEthnicGroupCodes   []CD                    `xml:"urn:hl7-org:sdtc ethnicGroupCode"`
	LanguageCommunications []LanguageCommunication `xml:"languageCommunication"`
	SDTCDeceasedInd        *BoolAttr               `xml:"urn:hl7-org:sdtc deceasedInd"`
	SDTCDeceasedTime       *TS                     `xml:"urn:hl7-org:sdtc deceasedTime"`
}

type BoolAttr struct {
	Value string `xml:"value,attr"`
}

type LanguageCommunication struct {
	LanguageCode  *CS       `xml:"languageCode"`
	PreferenceInd *BoolAttr `xml:"preferenceInd"`
}

type Author struct {
	Time           *TS             `xml:"time"`
	AssignedAuthor *AssignedAuthor `xml:"assignedAuthor"`
}

type AssignedAuthor struct {
	IDs                     []II          `xml:"id"`
	Code                    *CD           `xml:"code"`
	Addrs                   []AD          `xml:"addr"`
	Telecoms                []TEL         `xml:"telecom"`
	AssignedPerson          *Person       `xml:"assignedPerson"`
	RepresentedOrganization *Organization `xml:"representedOrganization"`
}

type Person struct {
	Names []EN `xml:"name"`
}

type Organization struct {
	IDs      []II     `xml:"id"`
	Names    []string `xml:"name"`
	Telecoms []TEL    `xml:"telecom"`
	Addrs    []AD     `xml:"addr"`
}

type Custodian struct {
	AssignedCustodian *AssignedCustodian `xml:"assignedCustodian"`
}

type AssignedCustodian struct {
	RepresentedCustodianOrganization *Organization `xml:"representedCustodianOrganization"`
}

type ComponentOf struct {
	EncompassingEncounter *EncompassingEncounter `xml:"encompassingEncounter"`
}

type EncompassingEncounter struct {
	IDs           []II `xml:"id"`
	Code          *CD  `xml:"code"`
	EffectiveTime *TS  `xml:"effectiveTime"`
}

type DocComponent struct {
	StructuredBody *StructuredBody `xml:"structuredBody"`
}

type StructuredBody struct {
	Components []BodyComponent `xml:"component"`
}

type BodyComponent struct {
	Section *Section `xml:"section"`
}

type Section struct {
	NullFlavor  string          `xml:"nullFlavor,attr"`
	TemplateIDs []II            `xml:"templateId"`
	ID          *II             `xml:"id"`
	Code        CD              `xml:"code"`
	Title       string          `xml:"title"`
	Text        *CDAText        `xml:"text"`
	Entries     []Entry         `xml:"entry"`
	Components  []BodyComponent `xml:"component"`
}

type Entry struct {
	TypeCode                string                   `xml:"typeCode,attr"`
	Act                     *Act                     `xml:"act"`
	Observation             *Observation             `xml:"observation"`
	SubstanceAdministration *SubstanceAdministration `xml:"substanceAdministration"`
	Procedure               *Procedure               `xml:"procedure"`
	Encounter               *EncounterAct            `xml:"encounter"`
	Organizer               *Organizer               `xml:"organizer"`
}

type EntryRelationship struct {
	TypeCode                string                   `xml:"typeCode,attr"`
	InversionInd            string                   `xml:"inversionInd,attr"`
	Act                     *Act                     `xml:"act"`
	Observation             *Observation             `xml:"observation"`
	SubstanceAdministration *SubstanceAdministration `xml:"substanceAdministration"`
	Procedure               *Procedure               `xml:"procedure"`
	Encounter               *EncounterAct            `xml:"encounter"`
	Organizer               *Organizer               `xml:"organizer"`
}

type Component struct {
	TypeCode    string       `xml:"typeCode,attr"`
	Observation *Observation `xml:"observation"`
	Organizer   *Organizer   `xml:"organizer"`
	Procedure   *Procedure   `xml:"procedure"`
}

type Act struct {
	ClassCode          string              `xml:"classCode,attr"`
	MoodCode           string              `xml:"moodCode,attr"`
	NullFlavor         string              `xml:"nullFlavor,attr"`
	NegationInd        string              `xml:"negationInd,attr"`
	TemplateIDs        []II                `xml:"templateId"`
	IDs                []II                `xml:"id"`
	Code               CD                  `xml:"code"`
	Text               *CDAText            `xml:"text"`
	StatusCode         *CS                 `xml:"statusCode"`
	EffectiveTime      *TS                 `xml:"effectiveTime"`
	Performers         []Performer         `xml:"performer"`
	Participants       []Participant       `xml:"participant"`
	EntryRelationships []EntryRelationship `xml:"entryRelationship"`
}

type Observation struct {
	ClassCode          string              `xml:"classCode,attr"`
	MoodCode           string              `xml:"moodCode,attr"`
	NullFlavor         string              `xml:"nullFlavor,attr"`
	NegationInd        string              `xml:"negationInd,attr"`
	TemplateIDs        []II                `xml:"templateId"`
	IDs                []II                `xml:"id"`
	Code               CD                  `xml:"code"`
	Text               *CDAText            `xml:"text"`
	StatusCode         *CS                 `xml:"statusCode"`
	EffectiveTime      *TS                 `xml:"effectiveTime"`
	Values             []CD                `xml:"value"`
	InterpretationCode []CD                `xml:"interpretationCode"`
	TargetSiteCodes    []CD                `xml:"targetSiteCode"`
	ReferenceRanges    []ReferenceRange    `xml:"referenceRange"`
	Performers         []Performer         `xml:"performer"`
	Participants       []Participant       `xml:"participant"`
	EntryRelationships []EntryRelationship `xml:"entryRelationship"`
}

type ReferenceRange struct {
	ObservationRange *ObservationRange `xml:"observationRange"`
}

type ObservationRange struct {
	Text  *CDAText `xml:"text"`
	Value *CD      `xml:"value"`
}

type SubstanceAdministration struct {
	ClassCode          string              `xml:"classCode,attr"`
	MoodCode           string              `xml:"moodCode,attr"`
	NullFlavor         string              `xml:"nullFlavor,attr"`
	NegationInd        string              `xml:"negationInd,attr"`
	TemplateIDs        []II                `xml:"templateId"`
	IDs                []II                `xml:"id"`
	Code               *CD                 `xml:"code"`
	Text               *CDAText            `xml:"text"`
	StatusCode         *CS                 `xml:"statusCode"`
	EffectiveTimes     []TS                `xml:"effectiveTime"`
	RouteCode          *CD                 `xml:"routeCode"`
	DoseQuantity       *CDABound           `xml:"doseQuantity"`
	Consumable         *Consumable         `xml:"consumable"`
	Performers         []Performer         `xml:"performer"`
	Participants       []Participant       `xml:"participant"`
	EntryRelationships []EntryRelationship `xml:"entryRelationship"`
}

type Consumable struct {
	ManufacturedProduct *ManufacturedProduct `xml:"manufacturedProduct"`
}

type ManufacturedProduct struct {
	TemplateIDs              []II                  `xml:"templateId"`
	IDs                      []II                  `xml:"id"`
	ManufacturedMaterial     *ManufacturedMaterial `xml:"manufacturedMaterial"`
	ManufacturerOrganization *Organization         `xml:"manufacturerOrganization"`
}

type ManufacturedMaterial struct {
	Code          CD     `xml:"code"`
	Name          string `xml:"name"`
	LotNumberText string `xml:"lotNumberText"`
}

type Procedure struct {
	ClassCode          string              `xml:"classCode,attr"`
	MoodCode           string              `xml:"moodCode,attr"`
	NullFlavor         string              `xml:"nullFlavor,attr"`
	NegationInd        string              `xml:"negationInd,attr"`
	TemplateIDs        []II                `xml:"templateId"`
	IDs                []II                `xml:"id"`
	Code               CD                  `xml:"code"`
	Text               *CDAText            `xml:"text"`
	StatusCode         *CS                 `xml:"statusCode"`
	EffectiveTime      *TS                 `xml:"effectiveTime"`
	TargetSiteCodes    []CD                `xml:"targetSiteCode"`
	Performers         []Performer         `xml:"performer"`
	Participants       []Participant       `xml:"participant"`
	EntryRelationships []EntryRelationship `xml:"entryRelationship"`
}

type EncounterAct struct {
	ClassCode          string              `xml:"classCode,attr"`
	MoodCode           string              `xml:"moodCode,attr"`
	NullFlavor         string              `xml:"nullFlavor,attr"`
	TemplateIDs        []II                `xml:"templateId"`
	IDs                []II                `xml:"id"`
	Code               *CD                 `xml:"code"`
	Text               *CDAText            `xml:"text"`
	StatusCode         *CS                 `xml:"statusCode"`
	EffectiveTime      *TS                 `xml:"effectiveTime"`
	Performers         []Performer         `xml:"performer"`
	Participants       []Participant       `xml:"participant"`
	EntryRelationships []EntryRelationship `xml:"entryRelationship"`
}

type Organizer struct {
	ClassCode     string      `xml:"classCode,attr"`
	MoodCode      string      `xml:"moodCode,attr"`
	NullFlavor    string      `xml:"nullFlavor,attr"`
	TemplateIDs   []II        `xml:"templateId"`
	IDs           []II        `xml:"id"`
	Code          *CD         `xml:"code"`
	StatusCode    *CS         `xml:"statusCode"`
	EffectiveTime *TS         `xml:"effectiveTime"`
	Components    []Component `xml:"component"`
	Performers    []Performer `xml:"performer"`
}

type Participant struct {
	TypeCode        string           `xml:"typeCode,attr"`
	ParticipantRole *ParticipantRole `xml:"participantRole"`
}

type ParticipantRole struct {
	ClassCode     string         `xml:"classCode,attr"`
	IDs           []II           `xml:"id"`
	Code          *CD            `xml:"code"`
	Addrs         []AD           `xml:"addr"`
	Telecoms      []TEL          `xml:"telecom"`
	PlayingEntity *PlayingEntity `xml:"playingEntity"`
}

type PlayingEntity struct {
	ClassCode string `xml:"classCode,attr"`
	Code      *CD    `xml:"code"`
	Names     []EN   `xml:"name"`
}

type Performer struct {
	TypeCode       string          `xml:"typeCode,attr"`
	AssignedEntity *AssignedEntity `xml:"assignedEntity"`
}

type AssignedEntity struct {
	IDs                     []II          `xml:"id"`
	Code                    *CD           `xml:"code"`
	Addrs                   []AD          `xml:"addr"`
	Telecoms                []TEL         `xml:"telecom"`
	AssignedPerson          *Person       `xml:"assignedPerson"`
	RepresentedOrganization *Organization `xml:"representedOrganization"`
}
