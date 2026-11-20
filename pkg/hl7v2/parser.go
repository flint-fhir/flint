// Package hl7v2 implements a pure-Go HL7 v2.x (v2.3 through v2.8) pipe-hat (ER7)
// parser, serializer, escape processor, acknowledgment (ACK/MSA/ERR) builder,
// MLLP v1 TCP framing server, and deterministic HL7 v2 -> FHIR R4 transaction
// Bundle clinical mapper.
package hl7v2

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxMessageBytes bounds the maximum accepted HL7 v2 ER7 payload size (10 MiB).
const MaxMessageBytes = 10 * 1024 * 1024

var (
	// ErrEmptyMessage indicates an empty or whitespace-only HL7 v2 payload.
	ErrEmptyMessage = errors.New("hl7v2: empty message")
	// ErrMessageTooLarge indicates an HL7 v2 payload exceeding MaxMessageBytes.
	ErrMessageTooLarge = errors.New("hl7v2: message exceeds maximum size limit")
	// ErrMissingMSH indicates the message does not start with an MSH segment header.
	ErrMissingMSH = errors.New("hl7v2: message must begin with MSH segment")
	// ErrInvalidDelimiters indicates malformed or colliding MSH-1/MSH-2 encoding characters.
	ErrInvalidDelimiters = errors.New("hl7v2: invalid MSH encoding characters")
	// ErrInvalidSegment indicates a segment with a malformed 3-character segment ID.
	ErrInvalidSegment = errors.New("hl7v2: invalid segment identifier")
)

// Delimiters holds the five HL7 v2 ER7 delimiter characters declared in MSH-1 and MSH-2.
type Delimiters struct {
	Field        byte // default '|'
	Component    byte // default '^'
	Repetition   byte // default '~'
	Escape       byte // default '\'
	Subcomponent byte // default '&'
}

// DefaultDelimiters returns standard HL7 v2 ER7 delimiters (|^~\&).
func DefaultDelimiters() Delimiters {
	return Delimiters{
		Field:        '|',
		Component:    '^',
		Repetition:   '~',
		Escape:       '\\',
		Subcomponent: '&',
	}
}

// EncodingChars returns the 4-character MSH-2 string (e.g. "^~\&").
func (d Delimiters) EncodingChars() string {
	return string([]byte{d.Component, d.Repetition, d.Escape, d.Subcomponent})
}

func (d Delimiters) withDefaults() Delimiters {
	def := DefaultDelimiters()
	if d.Field == 0 {
		d.Field = def.Field
	}
	if d.Component == 0 {
		d.Component = def.Component
	}
	if d.Repetition == 0 {
		d.Repetition = def.Repetition
	}
	if d.Escape == 0 {
		d.Escape = def.Escape
	}
	if d.Subcomponent == 0 {
		d.Subcomponent = def.Subcomponent
	}
	return d
}

func validateDelimiters(d Delimiters) error {
	chars := []byte{d.Field, d.Component, d.Repetition, d.Escape, d.Subcomponent}
	seen := make(map[byte]struct{}, len(chars))
	for _, c := range chars {
		if c < 0x21 || c > 0x7E {
			return fmt.Errorf("%w: non-printable ASCII delimiter 0x%02X", ErrInvalidDelimiters, c)
		}
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			return fmt.Errorf("%w: alphanumeric delimiter %q is not allowed", ErrInvalidDelimiters, c)
		}
		if _, dup := seen[c]; dup {
			return fmt.Errorf("%w: duplicate delimiter %q", ErrInvalidDelimiters, c)
		}
		seen[c] = struct{}{}
	}
	return nil
}

// Component represents a single HL7 v2 component containing 1..N unescaped subcomponents.
type Component struct {
	Subcomponents []string
}

// Value returns the primary (1st) subcomponent value.
func (c Component) Value() string {
	if len(c.Subcomponents) == 0 {
		return ""
	}
	return c.Subcomponents[0]
}

// Sub returns the 1-based subcomponent value.
func (c Component) Sub(idx int) string {
	if idx < 1 || idx > len(c.Subcomponents) {
		return ""
	}
	return c.Subcomponents[idx-1]
}

// Repetition represents a single repetition of an HL7 v2 field containing 1..N components.
type Repetition struct {
	Components []Component
}

// Value returns the first component's first subcomponent.
func (r Repetition) Value() string {
	return r.Comp(1)
}

// Comp returns the 1-based component's primary value.
func (r Repetition) Comp(idx int) string {
	if idx < 1 || idx > len(r.Components) {
		return ""
	}
	return r.Components[idx-1].Value()
}

// Sub returns the 1-based component and 1-based subcomponent value.
func (r Repetition) Sub(compIdx, subIdx int) string {
	if compIdx < 1 || compIdx > len(r.Components) {
		return ""
	}
	return r.Components[compIdx-1].Sub(subIdx)
}

// FormatJoined renders all non-empty components/subcomponents for plain text display.
func (r Repetition) FormatJoined(d Delimiters) string {
	d = d.withDefaults()
	compStrs := make([]string, len(r.Components))
	for i, c := range r.Components {
		compStrs[i] = strings.Join(c.Subcomponents, string(d.Subcomponent))
	}
	return strings.TrimRight(strings.Join(compStrs, string(d.Component)), string(d.Component))
}

// Field represents an HL7 v2 segment field containing 0..N repetitions.
type Field struct {
	Repetitions []Repetition
}

// IsEmpty reports whether all repetitions/components/subcomponents in the field are empty.
func (f Field) IsEmpty() bool {
	for _, rep := range f.Repetitions {
		for _, comp := range rep.Components {
			for _, sub := range comp.Subcomponents {
				if sub != "" {
					return false
				}
			}
		}
	}
	return true
}

// Value returns the first repetition's first component value.
func (f Field) Value() string {
	if len(f.Repetitions) == 0 {
		return ""
	}
	return f.Repetitions[0].Value()
}

// Comp returns the 1-based component of the first repetition.
func (f Field) Comp(compIdx int) string {
	if len(f.Repetitions) == 0 {
		return ""
	}
	return f.Repetitions[0].Comp(compIdx)
}

// Sub returns the 1-based component and 1-based subcomponent of the first repetition.
func (f Field) Sub(compIdx, subIdx int) string {
	if len(f.Repetitions) == 0 {
		return ""
	}
	return f.Repetitions[0].Sub(compIdx, subIdx)
}

// Segment represents a parsed HL7 v2 segment (such as MSH, PID, PV1, ORC, OBR, OBX, RXA).
// Fields is 0-indexed internally, while Field(seq) and Get(...) use standard 1-based HL7 v2
// field numbering (including MSH where MSH-1 is "|" and MSH-2 is "^~\&").
type Segment struct {
	Name   string
	Fields []Field
}

// Field returns the 1-based HL7 v2 field (e.g. seg.Field(3) is MSH-3 or PID-3).
func (s *Segment) Field(seq int) Field {
	if s == nil || seq < 1 || seq > len(s.Fields) {
		return Field{}
	}
	return s.Fields[seq-1]
}

// Get returns the unescaped value at 1-based (field, repetition, component, subcomponent).
func (s *Segment) Get(fieldSeq, repIdx, compIdx, subIdx int) string {
	if s == nil || fieldSeq < 1 || fieldSeq > len(s.Fields) {
		return ""
	}
	f := s.Fields[fieldSeq-1]
	if repIdx < 1 || repIdx > len(f.Repetitions) {
		return ""
	}
	return f.Repetitions[repIdx-1].Sub(compIdx, subIdx)
}

// Val is shorthand for s.Get(fieldSeq, 1, compIdx, 1).
func (s *Segment) Val(fieldSeq, compIdx int) string {
	return s.Get(fieldSeq, 1, compIdx, 1)
}

// Str is shorthand for s.Get(fieldSeq, 1, 1, 1).
func (s *Segment) Str(fieldSeq int) string {
	return s.Get(fieldSeq, 1, 1, 1)
}

// Message represents a parsed HL7 v2.x ER7 message.
type Message struct {
	Delimiters Delimiters
	Segments   []*Segment
}

// Segment returns the first segment matching name (e.g. "MSH", "PID", "PV1"), or nil.
func (m *Message) Segment(name string) *Segment {
	if m == nil {
		return nil
	}
	for _, seg := range m.Segments {
		if seg.Name == name {
			return seg
		}
	}
	return nil
}

// SegmentsByName returns all segments matching name in message order.
func (m *Message) SegmentsByName(name string) []*Segment {
	if m == nil {
		return nil
	}
	var out []*Segment
	for _, seg := range m.Segments {
		if seg.Name == name {
			out = append(out, seg)
		}
	}
	return out
}

// MessageType returns MSH-9.1 (message code, e.g. "ADT"), MSH-9.2 (trigger event, e.g. "A01"),
// and MSH-9.3 (message structure, e.g. "ADT_A01").
func (m *Message) MessageType() (code, trigger, structure string) {
	msh := m.Segment("MSH")
	if msh == nil {
		return "", "", ""
	}
	code = strings.TrimSpace(msh.Val(9, 1))
	trigger = strings.TrimSpace(msh.Val(9, 2))
	structure = strings.TrimSpace(msh.Val(9, 3))
	if trigger == "" && strings.Contains(code, "_") {
		parts := strings.SplitN(code, "_", 2)
		code, trigger = parts[0], parts[1]
	}
	return code, trigger, structure
}

// ControlID returns MSH-10 (Message Control ID).
func (m *Message) ControlID() string {
	if msh := m.Segment("MSH"); msh != nil {
		return strings.TrimSpace(msh.Str(10))
	}
	return ""
}

// ProcessingID returns MSH-11.1 (e.g. "P", "T", "D").
func (m *Message) ProcessingID() string {
	if msh := m.Segment("MSH"); msh != nil {
		return strings.TrimSpace(msh.Val(11, 1))
	}
	return ""
}

// Version returns MSH-12.1 (e.g. "2.5.1").
func (m *Message) Version() string {
	if msh := m.Segment("MSH"); msh != nil {
		return strings.TrimSpace(msh.Val(12, 1))
	}
	return ""
}

// SendingApplication returns MSH-3.1.
func (m *Message) SendingApplication() string {
	if msh := m.Segment("MSH"); msh != nil {
		return strings.TrimSpace(msh.Val(3, 1))
	}
	return ""
}

// SendingFacility returns MSH-4.1.
func (m *Message) SendingFacility() string {
	if msh := m.Segment("MSH"); msh != nil {
		return strings.TrimSpace(msh.Val(4, 1))
	}
	return ""
}

// ReceivingApplication returns MSH-5.1.
func (m *Message) ReceivingApplication() string {
	if msh := m.Segment("MSH"); msh != nil {
		return strings.TrimSpace(msh.Val(5, 1))
	}
	return ""
}

// ReceivingFacility returns MSH-6.1.
func (m *Message) ReceivingFacility() string {
	if msh := m.Segment("MSH"); msh != nil {
		return strings.TrimSpace(msh.Val(6, 1))
	}
	return ""
}

// MessageDateTime returns MSH-7.1 raw timestamp string.
func (m *Message) MessageDateTime() string {
	if msh := m.Segment("MSH"); msh != nil {
		return strings.TrimSpace(msh.Val(7, 1))
	}
	return ""
}

// Parse parses a raw HL7 v2.x ER7 string or byte slice into a structured Message.
func Parse(raw string) (*Message, error) {
	if len(raw) > MaxMessageBytes {
		return nil, ErrMessageTooLarge
	}
	trimmed := strings.TrimLeft(raw, " \t\r\n")
	if trimmed == "" {
		return nil, ErrEmptyMessage
	}
	if len(trimmed) < 8 || trimmed[:3] != "MSH" {
		return nil, ErrMissingMSH
	}

	d := DefaultDelimiters()
	d.Field = trimmed[3]
	encEnd := strings.IndexByte(trimmed[4:], d.Field)
	var encField string
	if encEnd < 0 {
		lineEnd := strings.IndexAny(trimmed[4:], "\r\n")
		if lineEnd < 0 {
			encField = trimmed[4:]
		} else {
			encField = trimmed[4 : 4+lineEnd]
		}
	} else {
		encField = trimmed[4 : 4+encEnd]
	}
	if len(encField) < 4 {
		return nil, fmt.Errorf("%w: MSH-2 must declare at least 4 encoding characters, got %q", ErrInvalidDelimiters, encField)
	}
	d.Component = encField[0]
	d.Repetition = encField[1]
	d.Escape = encField[2]
	d.Subcomponent = encField[3]
	if err := validateDelimiters(d); err != nil {
		return nil, err
	}

	normalized := strings.ReplaceAll(trimmed, "\r\n", "\r")
	normalized = strings.ReplaceAll(normalized, "\n", "\r")
	lines := strings.Split(normalized, "\r")

	segments := make([]*Segment, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		seg, err := parseSegmentLine(line, d)
		if err != nil {
			return nil, err
		}
		segments = append(segments, seg)
	}
	if len(segments) == 0 || segments[0].Name != "MSH" {
		return nil, ErrMissingMSH
	}
	return &Message{
		Delimiters: d,
		Segments:   segments,
	}, nil
}

// ParseBytes parses raw UTF-8/ASCII HL7 v2 ER7 bytes.
func ParseBytes(raw []byte) (*Message, error) {
	if len(raw) > MaxMessageBytes {
		return nil, ErrMessageTooLarge
	}
	return Parse(string(raw))
}

func isValidSegmentName(name string) bool {
	if len(name) != 3 {
		return false
	}
	if name[0] < 'A' || name[0] > 'Z' {
		return false
	}
	for i := 1; i < 3; i++ {
		c := name[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func isHeaderSegment(name string) bool {
	return name == "MSH" || name == "FHS" || name == "BHS"
}

func parseSegmentLine(line string, d Delimiters) (*Segment, error) {
	if len(line) < 3 {
		return nil, fmt.Errorf("%w: %q", ErrInvalidSegment, line)
	}
	segName := line[:3]
	if !isValidSegmentName(segName) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidSegment, segName)
	}
	if len(line) > 3 && line[3] != d.Field {
		return nil, fmt.Errorf("%w: expected field separator %q after %s", ErrInvalidSegment, d.Field, segName)
	}

	if isHeaderSegment(segName) {
		if len(line) == 3 {
			return nil, fmt.Errorf("%w: %s missing encoding characters", ErrInvalidDelimiters, segName)
		}
		rest := line[4:]
		rawParts := strings.Split(rest, string(d.Field))
		fields := make([]Field, 0, len(rawParts)+1)
		// Field 1 is the literal field separator ("|")
		fields = append(fields, literalField(string(d.Field)))
		if len(rawParts) > 0 {
			// Field 2 is the literal encoding characters ("^~\&") without unescaping
			fields = append(fields, literalField(rawParts[0]))
			for _, rf := range rawParts[1:] {
				fields = append(fields, parseField(rf, d))
			}
		}
		return &Segment{Name: segName, Fields: fields}, nil
	}

	if len(line) <= 4 {
		return &Segment{Name: segName}, nil
	}
	rawParts := strings.Split(line[4:], string(d.Field))
	fields := make([]Field, len(rawParts))
	for i, rf := range rawParts {
		fields[i] = parseField(rf, d)
	}
	return &Segment{Name: segName, Fields: fields}, nil
}

func literalField(val string) Field {
	return Field{
		Repetitions: []Repetition{
			{
				Components: []Component{
					{Subcomponents: []string{val}},
				},
			},
		},
	}
}

func parseField(raw string, d Delimiters) Field {
	if raw == "" {
		return Field{}
	}
	rawReps := strings.Split(raw, string(d.Repetition))
	reps := make([]Repetition, len(rawReps))
	for i, rr := range rawReps {
		rawComps := strings.Split(rr, string(d.Component))
		comps := make([]Component, len(rawComps))
		for j, rc := range rawComps {
			rawSubs := strings.Split(rc, string(d.Subcomponent))
			subs := make([]string, len(rawSubs))
			for k, rs := range rawSubs {
				subs[k] = Unescape(rs, d)
			}
			comps[j] = Component{Subcomponents: subs}
		}
		reps[i] = Repetition{Components: comps}
	}
	return Field{Repetitions: reps}
}

// Unescape decodes HL7 v2 escape sequences (\F\, \S\, \T\, \R\, \E\, \.br\, \Xhh...\, \H\, \N\)
// using the message's active delimiters.
func Unescape(raw string, d Delimiters) string {
	d = d.withDefaults()
	esc := d.Escape
	if strings.IndexByte(raw, esc) < 0 {
		return raw
	}
	var sb strings.Builder
	sb.Grow(len(raw))
	for i := 0; i < len(raw); {
		if raw[i] != esc {
			sb.WriteByte(raw[i])
			i++
			continue
		}
		end := strings.IndexByte(raw[i+1:], esc)
		if end < 0 {
			sb.WriteByte(raw[i])
			i++
			continue
		}
		seq := raw[i+1 : i+1+end]
		switch {
		case seq == "F":
			sb.WriteByte(d.Field)
		case seq == "S":
			sb.WriteByte(d.Component)
		case seq == "T":
			sb.WriteByte(d.Subcomponent)
		case seq == "R":
			sb.WriteByte(d.Repetition)
		case seq == "E":
			sb.WriteByte(d.Escape)
		case seq == ".br" || seq == ".sp":
			sb.WriteByte('\n')
		case seq == "H" || seq == "N":
			// Highlight start/normal formatting markers strip cleanly in plain clinical text
		case len(seq) >= 3 && (seq[0] == 'X' || seq[0] == 'x') && len(seq[1:])%2 == 0:
			if decoded, err := hex.DecodeString(seq[1:]); err == nil {
				sb.Write(decoded)
			} else {
				sb.WriteByte(esc)
				sb.WriteString(seq)
				sb.WriteByte(esc)
			}
		default:
			sb.WriteByte(esc)
			sb.WriteString(seq)
			sb.WriteByte(esc)
		}
		i += end + 2
	}
	return sb.String()
}

// Escape encodes special delimiter characters, newlines, and control characters into
// canonical HL7 v2 escape sequences (\F\, \S\, \T\, \R\, \E\, \.br\, \Xhh\).
func Escape(val string, d Delimiters) string {
	if val == "" {
		return ""
	}
	d = d.withDefaults()
	escStr := string(d.Escape)
	var sb strings.Builder
	sb.Grow(len(val))
	for i := 0; i < len(val); i++ {
		b := val[i]
		switch b {
		case d.Escape:
			sb.WriteString(escStr + "E" + escStr)
		case d.Field:
			sb.WriteString(escStr + "F" + escStr)
		case d.Component:
			sb.WriteString(escStr + "S" + escStr)
		case d.Subcomponent:
			sb.WriteString(escStr + "T" + escStr)
		case d.Repetition:
			sb.WriteString(escStr + "R" + escStr)
		case '\n':
			sb.WriteString(escStr + ".br" + escStr)
		case '\r':
			if i+1 < len(val) && val[i+1] == '\n' {
				continue
			}
			sb.WriteString(escStr + "X0D" + escStr)
		default:
			if b < 0x20 {
				fmt.Fprintf(&sb, "%sX%02X%s", escStr, b, escStr)
			} else {
				sb.WriteByte(b)
			}
		}
	}
	return sb.String()
}

// Serialize renders the Message back into canonical CR-terminated (0x0D) HL7 v2 ER7 wire format.
func (m *Message) Serialize() string {
	if m == nil || len(m.Segments) == 0 {
		return ""
	}
	d := m.Delimiters.withDefaults()
	var sb strings.Builder
	for _, seg := range m.Segments {
		if seg == nil || seg.Name == "" {
			continue
		}
		sb.WriteString(seg.Name)
		if isHeaderSegment(seg.Name) {
			sb.WriteByte(d.Field)
			sb.WriteString(d.EncodingChars())
			for idx := 2; idx < len(seg.Fields); idx++ {
				sb.WriteByte(d.Field)
				sb.WriteString(serializeField(seg.Fields[idx], d))
			}
		} else {
			for idx := 0; idx < len(seg.Fields); idx++ {
				sb.WriteByte(d.Field)
				sb.WriteString(serializeField(seg.Fields[idx], d))
			}
		}
		sb.WriteByte('\r')
	}
	return sb.String()
}

func serializeField(f Field, d Delimiters) string {
	if len(f.Repetitions) == 0 {
		return ""
	}
	repStrs := make([]string, len(f.Repetitions))
	for i, rep := range f.Repetitions {
		compStrs := make([]string, len(rep.Components))
		for j, comp := range rep.Components {
			subStrs := make([]string, len(comp.Subcomponents))
			for k, sub := range comp.Subcomponents {
				subStrs[k] = Escape(sub, d)
			}
			compStrs[j] = strings.Join(subStrs, string(d.Subcomponent))
		}
		repStrs[i] = strings.Join(compStrs, string(d.Component))
	}
	return strings.Join(repStrs, string(d.Repetition))
}

// ParseTimestamp converts an HL7 v2 TS/DTM string (YYYY[MM[DD[HH[MM[SS[.S[S[S[S]]]]]]]]][+/-ZZZZ])
// into an ISO-8601 FHIR R4 date or dateTime string.
// When asDateOnly is true, returns at most YYYY-MM-DD (or YYYY-MM / YYYY).
// When asDateOnly is false and a time component is present without an explicit offset,
// UTC ("Z") is appended as required by FHIR R4 dateTime / instant grammar.
func ParseTimestamp(raw string, asDateOnly bool) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	// Strip trailing degree-of-precision marker if present after '^'
	if idx := strings.IndexByte(s, '^'); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}

	var tzSuffix string
	if strings.HasSuffix(s, "Z") || strings.HasSuffix(s, "z") {
		tzSuffix = "Z"
		s = s[:len(s)-1]
	} else if plusIdx := strings.IndexAny(s, "+-"); plusIdx >= 4 {
		tzRaw := s[plusIdx:]
		s = s[:plusIdx]
		if len(tzRaw) == 5 && isAllDigits(tzRaw[1:]) {
			tzSuffix = tzRaw[:3] + ":" + tzRaw[3:]
		} else if len(tzRaw) == 3 && isAllDigits(tzRaw[1:]) {
			tzSuffix = tzRaw + ":00"
		} else {
			return "", false
		}
	}

	var frac string
	if dotIdx := strings.IndexByte(s, '.'); dotIdx >= 0 {
		fracPart := s[dotIdx+1:]
		if fracPart == "" || !isAllDigits(fracPart) {
			return "", false
		}
		if len(fracPart) > 4 {
			fracPart = fracPart[:4]
		}
		frac = "." + fracPart
		s = s[:dotIdx]
	}

	if !isAllDigits(s) || len(s) < 4 {
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
	if asDateOnly || len(s) == 8 {
		return dateStr, true
	}

	if len(s) != 10 && len(s) != 12 && len(s) != 14 {
		return "", false
	}
	hour := s[8:10]
	if h := atoi2(hour); h > 23 {
		return "", false
	}
	minStr := "00"
	if len(s) >= 12 {
		minStr = s[10:12]
		if mn := atoi2(minStr); mn > 59 {
			return "", false
		}
	}
	secStr := "00"
	if len(s) == 14 {
		secStr = s[12:14]
		if sc := atoi2(secStr); sc > 60 {
			return "", false
		}
	}
	if tzSuffix == "" {
		tzSuffix = "Z"
	}
	dateTimeStr := fmt.Sprintf("%sT%s:%s:%s%s%s", dateStr, hour, minStr, secStr, frac, tzSuffix)
	if _, err := time.Parse(time.RFC3339Nano, dateTimeStr); err != nil {
		return "", false
	}
	return dateTimeStr, true
}

func isAllDigits(s string) bool {
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
