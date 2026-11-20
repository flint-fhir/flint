package hl7v2

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// HL7 v2 Table 0008 Acknowledgment Codes.
const (
	ACKAccept = "AA" // Application Accept
	ACKError  = "AE" // Application Error
	ACKReject = "AR" // Application Reject
)

// BuildACK constructs an HL7 v2 ACK message (MSH + MSA + optional ERR) responding to req
// (or best-effort MSH metadata extracted from rawFallback when Parse failed).
func BuildACK(req *Message, rawFallback string, ackCode string, textMessage string, errCode string, now time.Time) string {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	switch ackCode {
	case ACKAccept, ACKError, ACKReject:
	default:
		ackCode = ACKError
	}

	d := DefaultDelimiters()
	sendApp := "FLINT"
	sendFac := "FLINT_FHIR"
	recvApp := ""
	recvFac := ""
	trigger := ""
	controlID := "UNKNOWN"
	procID := "P"
	version := "2.5.1"

	if req != nil {
		d = req.Delimiters.withDefaults()
		if v := req.ReceivingApplication(); v != "" {
			sendApp = v
		}
		if v := req.ReceivingFacility(); v != "" {
			sendFac = v
		}
		recvApp = req.SendingApplication()
		recvFac = req.SendingFacility()
		_, trigger, _ = req.MessageType()
		if v := req.ControlID(); v != "" {
			controlID = v
		}
		if v := req.ProcessingID(); v != "" {
			procID = v
		}
		if v := req.Version(); v != "" {
			version = v
		}
	} else if trimmed := strings.TrimLeft(rawFallback, " \t\r\n"); strings.HasPrefix(trimmed, "MSH") && len(trimmed) >= 8 {
		cand := Delimiters{
			Field:        trimmed[3],
			Component:    trimmed[4],
			Repetition:   trimmed[5],
			Escape:       trimmed[6],
			Subcomponent: trimmed[7],
		}
		if validateDelimiters(cand) == nil {
			d = cand
		}
		lineEnd := strings.IndexAny(trimmed, "\r\n")
		mshLine := trimmed
		if lineEnd >= 0 {
			mshLine = trimmed[:lineEnd]
		}
		parts := strings.Split(mshLine, string(d.Field))
		// parts[0]="MSH", parts[1]="^~\&", parts[2]=MSH-3, parts[3]=MSH-4, parts[4]=MSH-5, parts[5]=MSH-6,
		// parts[8]=MSH-9, parts[9]=MSH-10, parts[10]=MSH-11, parts[11]=MSH-12
		if len(parts) > 2 {
			recvApp = firstComp(parts[2], d.Component)
		}
		if len(parts) > 3 {
			recvFac = firstComp(parts[3], d.Component)
		}
		if len(parts) > 4 && firstComp(parts[4], d.Component) != "" {
			sendApp = firstComp(parts[4], d.Component)
		}
		if len(parts) > 5 && firstComp(parts[5], d.Component) != "" {
			sendFac = firstComp(parts[5], d.Component)
		}
		if len(parts) > 9 && strings.TrimSpace(parts[9]) != "" {
			controlID = strings.TrimSpace(parts[9])
		}
		if len(parts) > 10 && firstComp(parts[10], d.Component) != "" {
			procID = firstComp(parts[10], d.Component)
		}
		if len(parts) > 11 && firstComp(parts[11], d.Component) != "" {
			version = firstComp(parts[11], d.Component)
		}
	}

	ts := now.Format("20060102150405")
	sum := sha256.Sum256([]byte(controlID + "|" + ackCode + "|" + ts))
	ackCtrlID := "ACK-" + hex.EncodeToString(sum[:6])

	msh9Comp := []Component{{Subcomponents: []string{"ACK"}}}
	if trigger != "" {
		msh9Comp = []Component{
			{Subcomponents: []string{"ACK"}},
			{Subcomponents: []string{trigger}},
			{Subcomponents: []string{"ACK"}},
		}
	}

	msh := &Segment{
		Name: "MSH",
		Fields: []Field{
			literalField(string(d.Field)),
			literalField(d.EncodingChars()),
			literalField(sendApp),
			literalField(sendFac),
			literalField(recvApp),
			literalField(recvFac),
			literalField(ts),
			{},
			{Repetitions: []Repetition{{Components: msh9Comp}}},
			literalField(ackCtrlID),
			literalField(procID),
			literalField(version),
		},
	}

	cleanText := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(textMessage, "\r", " "), "\n", " "))
	msaFields := []Field{
		literalField(ackCode),
		literalField(controlID),
	}
	if cleanText != "" {
		msaFields = append(msaFields, literalField(cleanText))
	}
	msa := &Segment{
		Name:   "MSA",
		Fields: msaFields,
	}

	segs := []*Segment{msh, msa}
	if ackCode != ACKAccept && (cleanText != "" || errCode != "") {
		hl7ErrCode := strings.TrimSpace(errCode)
		if hl7ErrCode == "" {
			if ackCode == ACKReject {
				hl7ErrCode = "200"
			} else {
				hl7ErrCode = "207"
			}
		}
		errDesc := "Application internal error"
		switch hl7ErrCode {
		case "100":
			errDesc = "Segment sequence error"
		case "101":
			errDesc = "Required field missing"
		case "102":
			errDesc = "Data type error"
		case "200":
			errDesc = "Unsupported message type"
		}
		errSeg := &Segment{
			Name: "ERR",
			Fields: []Field{
				{},
				{},
				{
					Repetitions: []Repetition{
						{
							Components: []Component{
								{Subcomponents: []string{hl7ErrCode}},
								{Subcomponents: []string{errDesc}},
								{Subcomponents: []string{"HL70357"}},
							},
						},
					},
				},
				literalField("E"),
				{},
				{},
				literalField(cleanText),
			},
		}
		segs = append(segs, errSeg)
	}

	out := &Message{
		Delimiters: d,
		Segments:   segs,
	}
	return out.Serialize()
}

func firstComp(raw string, compSep byte) string {
	if idx := strings.IndexByte(raw, compSep); idx >= 0 {
		return strings.TrimSpace(raw[:idx])
	}
	return strings.TrimSpace(raw)
}
