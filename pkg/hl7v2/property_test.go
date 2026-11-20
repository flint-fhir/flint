package hl7v2_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"hegel.dev/go/hegel"

	"github.com/flint-fhir/flint/pkg/hl7v2"
	"github.com/flint-fhir/flint/pkg/validation"
)

func TestProperty_HL7v2EscapeAndMessageRoundTrip(t *testing.T) {
	delims := hl7v2.DefaultDelimiters()

	hegel.Test(t, func(ht *hegel.T) {
		rawText := hegel.Draw(ht, hegel.Text())
		if len(rawText) > 48 {
			rawText = rawText[:48]
		}
		// Replace valid ASCII/punctuation mixes alongside HL7 special delimiters
		useDelim := hegel.Draw(ht, hegel.SampledFrom([]string{"|", "^", "~", "\\", "&", "\n", "plain"}))
		val := strings.ToValidUTF8(rawText, "x") + useDelim + "clin"
		// Normalize bare '\r' since Escape normalizes '\r\n' -> '\r' -> \X0D\ cleanly,
		// and compare CR-free strings directly so exact string equality holds.
		val = strings.ReplaceAll(val, "\r", "")

		esc := hl7v2.Escape(val, delims)
		unesc := hl7v2.Unescape(esc, delims)
		if unesc != val {
			ht.Fatalf("escape round-trip mismatch: orig=%q esc=%q unesc=%q", val, esc, unesc)
		}

		mrn := hegel.Draw(ht, hegel.FromRegex(`[A-Z0-9]{4,10}`, true))
		family := hegel.Draw(ht, hegel.FromRegex(`[A-Z][a-z]{2,10}`, true))
		given := hegel.Draw(ht, hegel.FromRegex(`[A-Z][a-z]{2,10}`, true))
		ctrl := hegel.Draw(ht, hegel.FromRegex(`MSG-[0-9]{4}`, true))

		rawMsg := fmt.Sprintf(
			"MSH|^~\\&|EHR|FAC|FLINT|FLINT|20260115103000-0500||ADT^A01|%s|P|2.5.1\r"+
				"PID|1||%s^^^HOSP^MR||%s^%s||19850520|F\r"+
				"NTE|1|L|%s\r",
			ctrl, mrn, family, given, esc,
		)
		m1, err := hl7v2.Parse(rawMsg)
		if err != nil {
			ht.Fatalf("Parse failed: %v\nraw=%q", err, rawMsg)
		}
		if got := m1.Segment("NTE").Str(3); got != val {
			ht.Fatalf("NTE-3 unescaped mismatch: got=%q want=%q", got, val)
		}
		s1 := m1.Serialize()
		m2, err := hl7v2.Parse(s1)
		if err != nil {
			ht.Fatalf("re-Parse failed: %v", err)
		}
		if s2 := m2.Serialize(); s1 != s2 {
			ht.Fatalf("Serialize not idempotent:\ns1=%q\ns2=%q", s1, s2)
		}
	})
}

func TestProperty_HL7v2DeterministicIdempotentMapping(t *testing.T) {
	engine := validation.NewEngine(validation.DefaultOptions())
	fixedNow := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	hegel.Test(t, func(ht *hegel.T) {
		msgKind := hegel.Draw(ht, hegel.SampledFrom([]string{"ADT^A01", "ORU^R01", "VXU^V04", "ORM^O01"}))
		mrn := hegel.Draw(ht, hegel.FromRegex(`MRN[0-9]{4,8}`, true))
		ctrl1 := hegel.Draw(ht, hegel.FromRegex(`C1-[0-9]{4}`, true))
		ctrl2 := hegel.Draw(ht, hegel.FromRegex(`C2-[0-9]{4}`, true))
		family := hegel.Draw(ht, hegel.FromRegex(`[A-Z][a-z]{2,10}`, true))
		given := hegel.Draw(ht, hegel.FromRegex(`[A-Z][a-z]{2,10}`, true))
		gender := hegel.Draw(ht, hegel.SampledFrom([]string{"M", "F", "O", "U"}))
		year := hegel.Draw(ht, hegel.Integers(1940, 2020))
		month := hegel.Draw(ht, hegel.Integers(1, 12))
		day := hegel.Draw(ht, hegel.Integers(1, 28))
		dob := fmt.Sprintf("%04d%02d%02d", year, month, day)

		buildBody := func(ctrl string) string {
			hdr := fmt.Sprintf("MSH|^~\\&|SRC|FAC|DST|FAC|20260301100000Z||%s|%s|P|2.5.1\rPID|1||%s^^^FAC^MR||%s^%s||%s|%s\r",
				msgKind, ctrl, mrn, family, given, dob, gender)
			switch msgKind {
			case "ADT^A01":
				return hdr + "PV1|1|I|WARD^101^A|||||||||||||||VN100\r"
			case "ORU^R01":
				val := hegel.Draw(ht, hegel.Integers(40, 200))
				return hdr + fmt.Sprintf("OBR|1|PL1|FL1|2345-7^Glucose^LN|||202603010900Z\rOBX|1|NM|2345-7^Glucose^LN|1|%d|mg/dL||N|||F\r", val)
			case "VXU^V04":
				return hdr + "ORC|RE||IZ100\rRXA|0|1|202603011000Z|202603011000Z|208^COVID^CVX|0.3|mL^mL^UCUM||00\r"
			default:
				return hdr + "ORC|NW|PL2|FL2||IP\rOBR|1|PL2|FL2|4548-4^HbA1c^LN\r"
			}
		}

		msgA, err := hl7v2.Parse(buildBody(ctrl1))
		if err != nil {
			ht.Fatalf("Parse msgA failed: %v", err)
		}
		resA, err := hl7v2.MapToFHIRBundle(msgA, hl7v2.MapOptions{Now: fixedNow})
		if err != nil {
			ht.Fatalf("MapToFHIRBundle msgA failed: %v", err)
		}
		msgB, err := hl7v2.Parse(buildBody(ctrl2))
		if err != nil {
			ht.Fatalf("Parse msgB failed: %v", err)
		}
		resB, err := hl7v2.MapToFHIRBundle(msgB, hl7v2.MapOptions{Now: fixedNow})
		if err != nil {
			ht.Fatalf("MapToFHIRBundle msgB failed: %v", err)
		}

		if resA.PatientID != resB.PatientID {
			ht.Fatalf("Patient ID changed when only control ID differed: %q vs %q", resA.PatientID, resB.PatientID)
		}

		entries, _ := resA.Bundle["entry"].([]any)
		for _, e := range entries {
			rm := e.(map[string]any)["resource"].(map[string]any)
			rt := rm["resourceType"].(string)
			out := engine.ValidateResource(rt, rm)
			if !out.IsValid() {
				ht.Fatalf("invalid FHIR %s mapped from %s: %+v", rt, msgKind, out.Issues)
			}
		}
	})
}
