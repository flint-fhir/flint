package hl7v2_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/flint-fhir/flint/pkg/fhirutil"
	"github.com/flint-fhir/flint/pkg/hl7v2"
	"github.com/flint-fhir/flint/pkg/validation"
)

func loadGoldVector(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(b)
}

func validateMappedBundleStrict(t *testing.T, bundle map[string]any) map[string][]map[string]any {
	t.Helper()
	assert.Equal(t, "Bundle", bundle["resourceType"])
	assert.Equal(t, "transaction", bundle["type"])

	engine := validation.NewEngine(validation.DefaultOptions())
	descs := fhirutil.AllR4ResourceDescriptors()

	bundleBytes, err := json.Marshal(bundle)
	require.NoError(t, err)
	bundleMD := fhirutil.BundleDescriptor()
	normBundle, err := fhirutil.NormalizeFHIRJSONToProtoJSON(bundleMD, bundleBytes)
	require.NoError(t, err)
	dynBundle := dynamicpb.NewMessage(bundleMD)
	require.NoError(t, (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(normBundle, dynBundle))
	_, err = proto.Marshal(dynBundle)
	require.NoError(t, err)

	entries, ok := bundle["entry"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, entries)

	byType := make(map[string][]map[string]any)
	for _, rawEntry := range entries {
		em, ok := rawEntry.(map[string]any)
		require.True(t, ok)
		res, ok := em["resource"].(map[string]any)
		require.True(t, ok)
		rt, _ := res["resourceType"].(string)
		require.NotEmpty(t, rt)

		outcome := engine.ValidateResource(rt, res)
		require.True(t, outcome.IsValid(), "resource %s/%v failed FHIR R4 validation: %+v", rt, res["id"], outcome.Issues)

		if md, hasMD := descs[rt]; hasMD {
			resBytes, err := json.Marshal(res)
			require.NoError(t, err)
			normRes, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, resBytes)
			require.NoError(t, err)
			dynRes := dynamicpb.NewMessage(md)
			require.NoError(t, (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(normRes, dynRes),
				"resource %s failed google/fhir protojson.Unmarshal: %s", rt, string(resBytes))
			protoWire, err := proto.Marshal(dynRes)
			require.NoError(t, err)
			rtJSON, err := fhirutil.DenormalizeProtoJSONToFHIRJSON(md, rt, protoWire, 0, time.Time{})
			require.NoError(t, err)
			var origMap, rtMap map[string]any
			require.NoError(t, json.Unmarshal(resBytes, &origMap))
			require.NoError(t, json.Unmarshal(rtJSON, &rtMap))
			assert.Len(t, rtMap, len(origMap), "field count changed after Proto3 round-trip for %s:\norig=%s\nrt=%s", rt, string(resBytes), string(rtJSON))
		}
		byType[rt] = append(byType[rt], res)
	}
	return byType
}

func TestGoldNISTVectors_ParseMapAndValidate(t *testing.T) {
	fixedNow := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	adtRaw := loadGoldVector(t, "nist_adt_a01.hl7")
	adtMsg, err := hl7v2.Parse(adtRaw)
	require.NoError(t, err)
	code, trig, structCode := adtMsg.MessageType()
	assert.Equal(t, "ADT", code)
	assert.Equal(t, "A01", trig)
	assert.Equal(t, "ADT_A01", structCode)
	assert.Equal(t, "NIST-ADT-A01-0001", adtMsg.ControlID())
	assert.Equal(t, "P", adtMsg.ProcessingID())
	assert.Equal(t, "2.5.1", adtMsg.Version())
	assert.Equal(t, "EPIC_ADT", adtMsg.SendingApplication())
	assert.Equal(t, "METRO_GENERAL", adtMsg.SendingFacility())
	assert.Equal(t, "FLINT_EHR", adtMsg.ReceivingApplication())
	assert.Equal(t, "FLINT_CLINIC", adtMsg.ReceivingFacility())
	assert.Equal(t, "20260115083000-0500", adtMsg.MessageDateTime())
	assert.Equal(t, "742 Evergreen Terrace^Apt 2B", adtMsg.Segment("PID").Val(11, 1))

	adtRes, err := hl7v2.MapToFHIRBundle(adtMsg, hl7v2.MapOptions{Now: fixedNow})
	require.NoError(t, err)
	adtByType := validateMappedBundleStrict(t, adtRes.Bundle)
	require.Len(t, adtByType["Organization"], 1)
	require.Len(t, adtByType["Patient"], 1)
	require.Len(t, adtByType["RelatedPerson"], 1)
	require.Len(t, adtByType["Location"], 1)
	require.Len(t, adtByType["Encounter"], 1)
	require.Len(t, adtByType["AllergyIntolerance"], 2)
	require.Len(t, adtByType["Condition"], 2)
	require.Len(t, adtByType["Practitioner"], 2)
	require.Len(t, adtByType["Provenance"], 1)

	pat := adtByType["Patient"][0]
	assert.Equal(t, "female", pat["gender"])
	assert.Equal(t, "1979-06-24", pat["birthDate"])
	exts, _ := pat["extension"].([]any)
	assert.Len(t, exts, 2) // us-core-race & us-core-ethnicity

	enc := adtByType["Encounter"][0]
	assert.Equal(t, "in-progress", enc["status"])
	encClass := enc["class"].(map[string]any)
	assert.Equal(t, "IMP", encClass["code"])

	// Idempotent mapping check: byte-for-byte identical JSON when fixed Now is supplied
	adtRes2, err := hl7v2.MapToFHIRBundle(adtMsg, hl7v2.MapOptions{Now: fixedNow})
	require.NoError(t, err)
	b1, err := json.Marshal(adtRes.Bundle)
	require.NoError(t, err)
	b2, err := json.Marshal(adtRes2.Bundle)
	require.NoError(t, err)
	assert.JSONEq(t, string(b1), string(b2))

	// ORU^R01 NIST LRI vector
	oruMsg, err := hl7v2.Parse(loadGoldVector(t, "nist_oru_r01_lri.hl7"))
	require.NoError(t, err)
	oruRes, err := hl7v2.MapToFHIRBundle(oruMsg, hl7v2.MapOptions{Now: fixedNow})
	require.NoError(t, err)
	assert.Equal(t, adtRes.PatientID, oruRes.PatientID)
	assert.Equal(t, adtRes.EncounterID, oruRes.EncounterID)

	oruByType := validateMappedBundleStrict(t, oruRes.Bundle)
	require.Len(t, oruByType["ServiceRequest"], 1)
	require.Len(t, oruByType["Specimen"], 1)
	require.Len(t, oruByType["DiagnosticReport"], 1)
	require.Len(t, oruByType["Observation"], 4)
	dr := oruByType["DiagnosticReport"][0]
	assert.Equal(t, "final", dr["status"])
	assert.Contains(t, dr["conclusion"], "Fasting specimen verified\nHemolysis index normal")
	obs0 := oruByType["Observation"][0]
	assert.Equal(t, 148.0, obs0["valueQuantity"].(map[string]any)["value"])
	assert.Contains(t, obs0["note"].([]any)[0].(map[string]any)["text"], "Elevated fasting glucose|recommend HbA1c")
	obs2 := oruByType["Observation"][2]
	assert.Equal(t, ">", obs2["valueQuantity"].(map[string]any)["comparator"])
	assert.Equal(t, 60.0, obs2["valueQuantity"].(map[string]any)["value"])
	obs3 := oruByType["Observation"][3]
	assert.NotNil(t, obs3["valueCodeableConcept"])

	// VXU^V04 NIST IZ vector
	vxuMsg, err := hl7v2.Parse(loadGoldVector(t, "nist_vxu_v04_iz.hl7"))
	require.NoError(t, err)
	vxuRes, err := hl7v2.MapToFHIRBundle(vxuMsg, hl7v2.MapOptions{Now: fixedNow})
	require.NoError(t, err)
	assert.Equal(t, adtRes.PatientID, vxuRes.PatientID)
	vxuByType := validateMappedBundleStrict(t, vxuRes.Bundle)
	require.Len(t, vxuByType["Immunization"], 1)
	require.Len(t, vxuByType["Observation"], 2)
	imm := vxuByType["Immunization"][0]
	assert.Equal(t, "completed", imm["status"])
	assert.Equal(t, true, imm["primarySource"])
	assert.Equal(t, "LOT-PFZ-2026A", imm["lotNumber"])
	assert.Equal(t, "2026-12-31", imm["expirationDate"])
	assert.Equal(t, 0.3, imm["doseQuantity"].(map[string]any)["value"])
	assert.Len(t, imm["vaccineCode"].(map[string]any)["coding"].([]any), 2)

	// ORM^O01 vector
	ormMsg, err := hl7v2.Parse(loadGoldVector(t, "nist_orm_o01.hl7"))
	require.NoError(t, err)
	ormRes, err := hl7v2.MapToFHIRBundle(ormMsg, hl7v2.MapOptions{Now: fixedNow})
	require.NoError(t, err)
	assert.Equal(t, adtRes.PatientID, ormRes.PatientID)
	assert.Equal(t, adtRes.EncounterID, ormRes.EncounterID)
	ormByType := validateMappedBundleStrict(t, ormRes.Bundle)
	require.Len(t, ormByType["ServiceRequest"], 1)
	sr := ormByType["ServiceRequest"][0]
	assert.Equal(t, "active", sr["status"])
	assert.Equal(t, "stat", sr["priority"])
	assert.NotEmpty(t, sr["reasonReference"])
}

func TestADTVariants_OBXTypes_VXUVXUCancel_CustomDelimiters(t *testing.T) {
	fixedNow := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)

	t.Run("ADT_A03 without PV1 and deceased indicators + ADT_A04 outpatient", func(t *testing.T) {
		a03 := "MSH|^~\\&|SYS|FAC|||20260201100000||ADT^A03|CTRL-A03|P|2.5.1\r" +
			"EVN|A03|20260201095500\r" +
			"PID|1||ALT-99^^^FAC^PI||Smith^John||19500101|M|" + strings.Repeat("|", 20) + "20260201093000|Y\r"
		msg, err := hl7v2.Parse(a03)
		require.NoError(t, err)
		res, err := hl7v2.MapToFHIRBundle(msg, hl7v2.MapOptions{Now: fixedNow})
		require.NoError(t, err)
		byType := validateMappedBundleStrict(t, res.Bundle)
		assert.Equal(t, "finished", byType["Encounter"][0]["status"])
		assert.Equal(t, "2026-02-01T09:30:00Z", byType["Patient"][0]["deceasedDateTime"])

		a04 := "MSH|^~\\&|SYS|FAC|||20260201100000||ADT^A04|CTRL-A04|P|2.5.1\r" +
			"PID|1||ALT-99^^^FAC^PI||Smith^John||19500101|M\r" +
			"PV1|1|O\r"
		msg4, err := hl7v2.Parse(a04)
		require.NoError(t, err)
		res4, err := hl7v2.MapToFHIRBundle(msg4, hl7v2.MapOptions{Now: fixedNow})
		require.NoError(t, err)
		byType4 := validateMappedBundleStrict(t, res4.Bundle)
		assert.Equal(t, "AMB", byType4["Encounter"][0]["class"].(map[string]any)["code"])

		// Deceased boolean branch (no PID-29, PID-30 = Y)
		a08 := "MSH|^~\\&|SYS|FAC|||20260201100000||ADT^A08|CTRL-A08|P|2.5.1\r" +
			"PID|1||ALT-99^^^FAC^PI||Smith^John||19500101|U|" + strings.Repeat("|", 21) + "Y\r"
		msg8, err := hl7v2.Parse(a08)
		require.NoError(t, err)
		res8, err := hl7v2.MapToFHIRBundle(msg8, hl7v2.MapOptions{Now: fixedNow})
		require.NoError(t, err)
		byType8 := validateMappedBundleStrict(t, res8.Bundle)
		assert.Equal(t, true, byType8["Patient"][0]["deceasedBoolean"])
	})

	t.Run("ORU extended OBX value types (SN range/ratio, DT, TS, TM, ST, TX, comparators)", func(t *testing.T) {
		oru := "MSH|^~\\&|LAB|FAC|||20260201100000||ORU^R01|CTRL-ORU-EXT|P|2.5.1\r" +
			"PID|1||MRN-EXT^^^FAC^MR||Doe^Jane||19900515|F\r" +
			"OBR|1|PL-1|FL-1|12345-6^Panel^LN|||202602010900\r" +
			"OBX|1|NM|1000-1^Titer^LN|1|<5|{titer}||L|||P\r" +
			"OBX|2|SN|1000-2^Range Test^LN|1|^10^-^20|mg/dL||A|||C\r" +
			"OBX|3|SN|1000-3^Ratio Test^LN|1|^1^:^128|||HH|||A\r" +
			"OBX|4|DT|1000-4^Onset Date^LN|1|20260120||||||W\r" +
			"OBX|5|TS|1000-5^Event TS^LN|1|20260120143000-0500||||||F\r" +
			"OBX|6|TM|1000-6^Collection Time^LN|1|083015||||||F\r" +
			"OBX|7|TX|1000-7^Narrative^LN|1|Line 1~Line 2||||||F\r"
		msg, err := hl7v2.Parse(oru)
		require.NoError(t, err)
		res, err := hl7v2.MapToFHIRBundle(msg, hl7v2.MapOptions{Now: fixedNow})
		require.NoError(t, err)
		byType := validateMappedBundleStrict(t, res.Bundle)
		obs := byType["Observation"]
		require.Len(t, obs, 7)
		assert.Equal(t, "<", obs[0]["valueQuantity"].(map[string]any)["comparator"])
		assert.NotNil(t, obs[1]["valueRange"])
		assert.NotNil(t, obs[2]["valueRatio"])
		assert.Equal(t, "2026-01-20", obs[3]["valueDateTime"])
		assert.Equal(t, "2026-01-20T14:30:00-05:00", obs[4]["valueDateTime"])
		assert.Equal(t, "08:30:15", obs[5]["valueTime"])
		assert.Equal(t, "Line 1\nLine 2", obs[6]["valueString"])
	})

	t.Run("VXU not-done refusal and historical secondary report + ORM cancelled/held", func(t *testing.T) {
		vxuRefusal := "MSH|^~\\&|EHR|FAC|||20260201100000||VXU^V04|CTRL-VXU-REF|P|2.5.1\r" +
			"PID|1||MRN-V^^^FAC^MR||Doe^Alex||20150101|O\r" +
			"PV1|1|N\r" +
			"ORC|RE||IZ-1\r" +
			"RXA|0|1|20260201|20260201|88^Influenza^CVX|999|||01^Historical information - source unspecified^NIP001|||||||||00^Parental decision^NIP002||RE\r"
		msg, err := hl7v2.Parse(vxuRefusal)
		require.NoError(t, err)
		res, err := hl7v2.MapToFHIRBundle(msg, hl7v2.MapOptions{Now: fixedNow})
		require.NoError(t, err)
		byType := validateMappedBundleStrict(t, res.Bundle)
		assert.Empty(t, byType["Encounter"])
		imm := byType["Immunization"][0]
		assert.Equal(t, "not-done", imm["status"])
		assert.Equal(t, false, imm["primarySource"])
		assert.NotNil(t, imm["reportOrigin"])
		assert.NotNil(t, imm["statusReason"])
		_, hasDose := imm["doseQuantity"]
		assert.False(t, hasDose)

		ormCancel := "MSH|^~\\&|CPOE|FAC|||20260201100000||ORM^O01|CTRL-ORM-CA|P|2.5.1\r" +
			"PID|1||MRN-V^^^FAC^MR||Doe^Alex||20150101|O\r" +
			"ORC|CA|PL-9|FL-9\r" +
			"OBR|1|PL-9|FL-9|2345-7^Glucose^LN\r" +
			"ORC|HD|PL-10|FL-10\r" +
			"OBR|2|PL-10|FL-10|2160-0^Creatinine^LN\r"
		msgORM, err := hl7v2.Parse(ormCancel)
		require.NoError(t, err)
		resORM, err := hl7v2.MapToFHIRBundle(msgORM, hl7v2.MapOptions{Now: fixedNow})
		require.NoError(t, err)
		byTypeORM := validateMappedBundleStrict(t, resORM.Bundle)
		require.Len(t, byTypeORM["ServiceRequest"], 2)
		assert.Equal(t, "revoked", byTypeORM["ServiceRequest"][0]["status"])
		assert.Equal(t, "on-hold", byTypeORM["ServiceRequest"][1]["status"])
	})

	t.Run("Custom delimiters escape and unescape round-trip", func(t *testing.T) {
		raw := "MSH#!@%*#APP#FAC#RAPP#RFAC#20260201100000##ADT!A01#C-1#P#2.5.1\n" +
			"PID#1##ID1###Smith!John%F%Jr#Smith*S*Line#19800101#M\n"
		msg, err := hl7v2.ParseBytes([]byte(raw))
		require.NoError(t, err)
		assert.Equal(t, byte('#'), msg.Delimiters.Field)
		assert.Equal(t, byte('!'), msg.Delimiters.Component)
		assert.Equal(t, byte('@'), msg.Delimiters.Repetition)
		assert.Equal(t, byte('%'), msg.Delimiters.Escape)
		assert.Equal(t, byte('*'), msg.Delimiters.Subcomponent)
		assert.Equal(t, "John#Jr", msg.Segment("PID").Get(6, 1, 2, 1))
		assert.Equal(t, "Smith", msg.Segment("PID").Get(7, 1, 1, 1))
		assert.Equal(t, "Line", msg.Segment("PID").Get(7, 1, 1, 3))

		ser := msg.Serialize()
		msg2, err := hl7v2.Parse(ser)
		require.NoError(t, err)
		assert.Equal(t, ser, msg2.Serialize())

		esc := hl7v2.Escape("A|B^C~D\\E&F\n\r\tG", hl7v2.DefaultDelimiters())
		assert.Contains(t, esc, `\F\`)
		assert.Contains(t, esc, `\S\`)
		assert.Contains(t, esc, `\R\`)
		assert.Contains(t, esc, `\E\`)
		assert.Contains(t, esc, `\T\`)
		assert.Contains(t, esc, `\.br\`)
		assert.Equal(t, "A|B^C~D\\E&F\n\r\tG", hl7v2.Unescape(esc, hl7v2.DefaultDelimiters()))
		assert.Equal(t, "raw\x41value", hl7v2.Unescape(`raw\X41\value`, hl7v2.DefaultDelimiters()))
	})
}

func TestParseAndMapperErrorPaths_AndACKBuilder(t *testing.T) {
	badInputs := []string{
		"",
		"   \r\n  ",
		"PID|1||123",
		"MSH|^^",
		"MSH|^~|APP",
		"MSH||~\\&|APP",
		"MSH|A~\\&|APP",
		"MSH|^~\\&|APP\r1BAD|x",
		"MSH|^~\\&|APP\rBAD_1|x",
	}
	for _, in := range badInputs {
		_, err := hl7v2.Parse(in)
		assert.Error(t, err, "expected error for %q", in)
	}

	oversized := make([]byte, hl7v2.MaxMessageBytes+1)
	_, err := hl7v2.ParseBytes(oversized)
	assert.ErrorIs(t, err, hl7v2.ErrMessageTooLarge)

	_, err = hl7v2.MapToFHIRBundle(nil, hl7v2.MapOptions{})
	assert.ErrorIs(t, err, hl7v2.ErrEmptyMessage)

	noPID, err := hl7v2.Parse("MSH|^~\\&|A|B|C|D|20260101||ADT^A01|1|P|2.5.1\rEVN|A01")
	require.NoError(t, err)
	_, err = hl7v2.MapToFHIRBundle(noPID, hl7v2.MapOptions{})
	require.ErrorContains(t, err, "PID")

	unsupported, err := hl7v2.Parse("MSH|^~\\&|A|B|C|D|20260101||QRY^A19|1|P|2.5.1\rPID|1||123")
	require.NoError(t, err)
	_, err = hl7v2.MapToFHIRBundle(unsupported, hl7v2.MapOptions{})
	assert.ErrorIs(t, err, hl7v2.ErrUnsupportedMessageType)

	// ACK generation tests
	fixedNow := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	ackAA := hl7v2.BuildACK(unsupported, "", hl7v2.ACKAccept, "Accepted", "", fixedNow)
	parsedACK, err := hl7v2.Parse(ackAA)
	require.NoError(t, err)
	assert.Equal(t, "AA", parsedACK.Segment("MSA").Str(1))
	assert.Equal(t, "1", parsedACK.Segment("MSA").Str(2))
	assert.Nil(t, parsedACK.Segment("ERR"))

	ackAR := hl7v2.BuildACK(nil, "MSH|^~\\&|SAPP|SFAC|RAPP|RFAC|20260101||QRY^A19|RAW-CTRL-9|T|2.4\rBROKEN", hl7v2.ACKReject, "bad\nmsg", "200", fixedNow)
	parsedAR, err := hl7v2.Parse(ackAR)
	require.NoError(t, err)
	assert.Equal(t, "AR", parsedAR.Segment("MSA").Str(1))
	assert.Equal(t, "RAW-CTRL-9", parsedAR.Segment("MSA").Str(2))
	assert.Equal(t, "RAPP", parsedAR.SendingApplication())
	assert.Equal(t, "SAPP", parsedAR.ReceivingApplication())
	require.NotNil(t, parsedAR.Segment("ERR"))
	assert.Equal(t, "200", parsedAR.Segment("ERR").Val(3, 1))

	// Timestamp edge cases
	_, ok := hl7v2.ParseTimestamp("", false)
	assert.False(t, ok)
	_, ok = hl7v2.ParseTimestamp("20261301", false)
	assert.False(t, ok)
	_, ok = hl7v2.ParseTimestamp("20260230", false)
	assert.False(t, ok)
	dt, ok := hl7v2.ParseTimestamp("2026", false)
	assert.True(t, ok)
	assert.Equal(t, "2026", dt)
	dt, ok = hl7v2.ParseTimestamp("202602", false)
	assert.True(t, ok)
	assert.Equal(t, "2026-02", dt)
	dt, ok = hl7v2.ParseTimestamp("20260215120000.1234+0000", false)
	assert.True(t, ok)
	assert.Equal(t, "2026-02-15T12:00:00.1234+00:00", dt)
}

func TestMLLP_FramingAndConcurrentServer(t *testing.T) {
	// Framing error paths
	_, err := hl7v2.ReadMLLPFrame(bufio.NewReader(bytes.NewReader([]byte("NO_VT_START"))), 1024)
	assert.ErrorIs(t, err, hl7v2.ErrInvalidMLLPFrame)

	_, err = hl7v2.ReadMLLPFrame(bufio.NewReader(bytes.NewReader([]byte("\x0BMSH|^~\\&|EOF"))), 1024)
	assert.ErrorIs(t, err, hl7v2.ErrInvalidMLLPFrame)

	_, err = hl7v2.ReadMLLPFrame(bufio.NewReader(bytes.NewReader([]byte("\x0BMSH|^~\\&\x1C"))), 1024)
	assert.ErrorIs(t, err, hl7v2.ErrInvalidMLLPFrame)

	_, err = hl7v2.ReadMLLPFrame(bufio.NewReader(bytes.NewReader([]byte("\x0BMSH|^~\\&\x1CX"))), 1024)
	assert.ErrorIs(t, err, hl7v2.ErrInvalidMLLPFrame)

	_, err = hl7v2.ReadMLLPFrame(bufio.NewReader(bytes.NewReader([]byte("\x0BMSH|^~\\&|TOO_BIG\x1C\r"))), 5)
	assert.ErrorIs(t, err, hl7v2.ErrMLLPFrameTooLarge)

	// Live TCP MLLP Server with default handler & custom handler + concurrency
	srv := hl7v2.NewMLLPServer(hl7v2.MLLPConfig{Addr: "127.0.0.1:0"})
	require.NoError(t, srv.Start())
	defer func() { _ = srv.Close() }()

	sendFrame := func(addr string, payload []byte) (*hl7v2.Message, error) {
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		if err := hl7v2.WriteMLLPFrame(conn, payload); err != nil {
			return nil, err
		}
		ackBytes, err := hl7v2.ReadMLLPFrame(bufio.NewReader(conn), hl7v2.MaxMessageBytes)
		if err != nil {
			return nil, err
		}
		return hl7v2.ParseBytes(ackBytes)
	}

	adt := loadGoldVector(t, "nist_adt_a01.hl7")
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ackMsg, err := sendFrame(srv.Addr(), []byte(adt))
			assert.NoError(t, err)
			if ackMsg != nil {
				assert.Equal(t, "AA", ackMsg.Segment("MSA").Str(1))
			}
		}()
	}
	wg.Wait()

	// Invalid frame triggers AR NACK cleanly
	conn, err := net.DialTimeout("tcp", srv.Addr(), 5*time.Second)
	require.NoError(t, err)
	_, err = conn.Write([]byte("BAD_START"))
	require.NoError(t, err)
	nackBytes, err := hl7v2.ReadMLLPFrame(bufio.NewReader(conn), hl7v2.MaxMessageBytes)
	_ = conn.Close()
	require.NoError(t, err)
	nackMsg, err := hl7v2.ParseBytes(nackBytes)
	require.NoError(t, err)
	assert.Equal(t, "AR", nackMsg.Segment("MSA").Str(1))

	// Error handler fallback ACK generation
	errSrv := hl7v2.NewMLLPServer(hl7v2.MLLPConfig{
		Addr: "127.0.0.1:0",
		Handler: func(_ context.Context, _ []byte) ([]byte, error) {
			return nil, errors.New("boom")
		},
	})
	require.NoError(t, errSrv.Start())
	defer func() { _ = errSrv.Close() }()
	aeMsg, err := sendFrame(errSrv.Addr(), []byte(adt))
	require.NoError(t, err)
	assert.Equal(t, "AE", aeMsg.Segment("MSA").Str(1))
}

func FuzzHL7v2Parser(f *testing.F) {
	for _, fn := range []string{"nist_adt_a01.hl7", "nist_oru_r01_lri.hl7", "nist_vxu_v04_iz.hl7", "nist_orm_o01.hl7"} {
		if b, err := os.ReadFile(filepath.Join("testdata", fn)); err == nil {
			f.Add(b)
		}
	}
	f.Add([]byte("MSH|^~\\&|A|B|C|D|20260101||ADT^A01|1|P|2.5.1\rPID|1||MRN1||Doe\\S\\J^Alice\r"))
	f.Add([]byte("MSH||~\\&|bad"))
	f.Add([]byte("\x0BMSH|^~\\&|A|B|C|D|20260101||ADT^A01|1|P|2.5.1\r\x1C\r"))
	f.Add([]byte("garbage"))

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64*1024 {
			return
		}
		_, _ = hl7v2.ReadMLLPFrame(bufio.NewReader(bytes.NewReader(data)), 64*1024)

		msg, err := hl7v2.ParseBytes(data)
		ack := hl7v2.BuildACK(msg, string(data), hl7v2.ACKAccept, "ok", "", time.Unix(1700000000, 0).UTC())
		if ackMsg, ackErr := hl7v2.Parse(ack); ackErr != nil || ackMsg.Segment("MSA") == nil {
			t.Fatalf("BuildACK produced unparseable ACK: %v\nack=%q", ackErr, ack)
		}
		if err != nil {
			return
		}
		ser := msg.Serialize()
		msg2, err2 := hl7v2.Parse(ser)
		if err2 != nil {
			t.Fatalf("re-parse of serialized HL7 v2 message failed: %v\nser=%q", err2, ser)
		}
		if msg2.Serialize() != ser {
			t.Fatalf("serialization not idempotent:\nfirst:  %q\nsecond: %q", ser, msg2.Serialize())
		}
		_, _ = hl7v2.MapToFHIRBundle(msg, hl7v2.MapOptions{Now: time.Unix(1700000000, 0).UTC()})
	})
}
