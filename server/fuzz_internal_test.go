package server

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/flint-fhir/flint/pkg/fhirutil"
	"github.com/flint-fhir/flint/pkg/validation"
	"github.com/flint-fhir/flint/store"
)

const fuzzCanonicalPatientJSON = `{
	"resourceType": "Patient",
	"id": "fuzz-pat-1",
	"active": true,
	"gender": "female",
	"birthDate": "1985-04-12",
	"name": [
		{"use": "official", "family": "Chalmers", "given": ["Peter", "James"]},
		{"use": "usual", "given": ["Jim"]}
	],
	"telecom": [
		{"system": "phone", "value": "(03) 5555 6473", "use": "work", "rank": 1}
	],
	"address": [
		{"use": "home", "line": ["534 Erewhon St"], "city": "PleasantVille", "state": "VIC", "postalCode": "3999"}
	],
	"identifier": [
		{"system": "http://hospital.example.org/mrn", "value": "MRN-FUZZ-001"}
	]
}`

// FuzzFHIRPathR4Engine fuzzes the HL7 FHIRPath R4 evaluator (evalFHIRPath) with
// arbitrary expressions to verify zero panics on malformed or adversarial syntax.
func FuzzFHIRPathR4Engine(f *testing.F) {
	seeds := []string{
		"Patient.name.where(use = 'official').given.first()",
		"name.given.count() > 1 and active = true",
		"(1 + 2 * 3) = 7 and (10 div 3) = 3 and (10 mod 3) = 1",
		"birthDate < @2000-01-01 and (@1985-04-12 + 10 years) = @1995-04-12",
		"name.select(family + ', ' + given.first()).distinct()",
		"telecom.where(system = 'phone').value.matches('^\\(03\\).*$')",
		"name.given.aggregate($this.length() + $total, 0)",
		"true implies false",
		"{}.empty() and true.not() = false",
		"address.line.replaceMatches('[0-9]+', '#')",
		"name.repeat(given)",
		"10.5 'kg' > 5.0 'kg'",
		"((((unclosed.paren.where(x = '1')",
		"name.where(use = 'unterminated string)",
		"name[-999999999999999999999]",
		"1 / 0",
		"1 div 0",
		"1 mod 0",
		"substring(-50, 999999)",
		"...",
		"|",
		"",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	var root map[string]any
	if err := json.Unmarshal([]byte(fuzzCanonicalPatientJSON), &root); err != nil {
		f.Fatalf("unmarshal canonical patient: %v", err)
	}

	f.Fuzz(func(t *testing.T, expr string) {
		_ = evalFHIRPath(root, expr)
	})
}

// FuzzPatchEngines fuzzes RFC 6902 JSON Patch, HL7 FHIRPath Parameters Patch,
// and RFC 7396 JSON Merge Patch against a canonical FHIR R4 Patient JSON payload.
func FuzzPatchEngines(f *testing.F) {
	seeds := []string{
		`[{"op":"replace","path":"/gender","value":"other"},{"op":"add","path":"/name/0/given/-","value":"Marie"}]`,
		`[{"op":"test","path":"/active","value":true},{"op":"remove","path":"/telecom/0"}]`,
		`[{"op":"move","from":"/name/1","path":"/name/0"},{"op":"copy","from":"/gender","path":"/birthDate"}]`,
		`[{"op":"replace","path":"/name/999999/family","value":"OutOfBounds"}]`,
		`[{"op":"add","path":"invalid-no-leading-slash","value":123}]`,
		`[{"op":"move","from":"/name","path":"/name/0/given"}]`,
		`{"resourceType":"Parameters","parameter":[{"name":"operation","part":[{"name":"type","valueCode":"replace"},{"name":"path","valueString":"Patient.gender"},{"name":"value","valueCode":"male"}]}]}`,
		`{"resourceType":"Parameters","parameter":[{"name":"operation","part":[{"name":"type","valueCode":"insert"},{"name":"path","valueString":"Patient.name"},{"name":"index","valueInteger":0},{"name":"value","valueHumanName":{"family":"Added"}}]}]}`,
		`{"resourceType":"Parameters","parameter":[{"name":"operation","part":[{"name":"type","valueCode":"delete"},{"name":"path","valueString":"Patient.telecom[0]"}}]}`,
		`{"gender":"unknown","telecom":null,"address":[{"city":"UpdatedCity"}]}`,
		`[`,
		`{"malformed":`,
		`null`,
		``,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	baseDoc := []byte(fuzzCanonicalPatientJSON)

	f.Fuzz(func(t *testing.T, patchPayload string) {
		raw := []byte(patchPayload)
		_, _, _ = ApplyJSONPatch(baseDoc, raw)
		_, _, _ = ApplyFHIRPatch(baseDoc, raw)
		_, _ = ApplyMergePatch(baseDoc, raw)
	})
}

// FuzzValidationAndDynamicExtraction fuzzes US Core / StructureDefinition JSON validation,
// ProtoJSON normalization, dynamicpb unmarshaling, and search index extraction.
func FuzzValidationAndDynamicExtraction(f *testing.F) {
	seeds := []string{
		fuzzCanonicalPatientJSON,
		`{"resourceType":"Observation","id":"obs-1","status":"final","code":{"coding":[{"system":"http://loinc.org","code":"8867-4"}]},"subject":{"reference":"Patient/fuzz-pat-1"},"effectiveDateTime":"2026-01-15T10:30:00Z","valueQuantity":{"value":72,"unit":"/min","system":"http://unitsofmeasure.org","code":"/min"}}`,
		`{"resourceType":"Patient","id":"invalid id with spaces","gender":"not-a-valid-gender","birthDate":"9999-99-99","unknownElement":123}`,
		`{"resourceType":"Patient","extension":[{"url":"http://hl7.org/fhir/us/core/StructureDefinition/us-core-race","extension":[{"url":"ombCategory","valueCoding":{"system":"urn:oid:2.16.840.1.113883.6.238","code":"2106-3"}}]}],"contained":[{"resourceType":"Organization","id":"org1","name":"Contained Org"}],"managingOrganization":{"reference":"#org1"}}`,
		`{"resourceType":123,"id":null,"name":"not-an-array"}`,
		`[]`,
		`{`,
		``,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	valEngine := validation.NewEngine(validation.DefaultOptions())
	descriptors := fhirutil.AllR4ResourceDescriptors()
	patMD := descriptors["Patient"]
	obsMD := descriptors["Observation"]
	patExtractor := store.NewDynamicResourceExtractor("Patient", patMD)
	obsExtractor := store.NewDynamicResourceExtractor("Observation", obsMD)

	f.Fuzz(func(t *testing.T, rawJSON string) {
		payload := []byte(rawJSON)
		for _, spec := range []struct {
			resType   string
			md        func()
			extractor store.IndexExtractorFunc
		}{
			{resType: "Patient", extractor: patExtractor},
			{resType: "Observation", extractor: obsExtractor},
		} {
			_ = valEngine.ValidateJSON(spec.resType, payload)
			md := patMD
			if spec.resType == "Observation" {
				md = obsMD
			}
			norm, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, payload)
			if err != nil {
				continue
			}
			msg := dynamicpb.NewMessage(md)
			unmarshaler := protojson.UnmarshalOptions{DiscardUnknown: true}
			if err := unmarshaler.Unmarshal(norm, msg); err != nil {
				continue
			}
			pb, err := proto.Marshal(msg)
			if err != nil {
				continue
			}
			_, _ = spec.extractor("fuzz-tenant", "fuzz-id", pb)
		}
	})
}

type fuzzWSNetConn struct {
	reader *bytes.Reader
}

func (c *fuzzWSNetConn) Read(b []byte) (int, error)         { return c.reader.Read(b) }
func (c *fuzzWSNetConn) Write(b []byte) (int, error)        { return len(b), nil }
func (c *fuzzWSNetConn) Close() error                       { return nil }
func (c *fuzzWSNetConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (c *fuzzWSNetConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (c *fuzzWSNetConn) SetDeadline(_ time.Time) error      { return nil }
func (c *fuzzWSNetConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *fuzzWSNetConn) SetWriteDeadline(_ time.Time) error { return nil }

func buildMaskedClientWSFrame(fin bool, opcode byte, mask [4]byte, payload []byte) []byte {
	var out bytes.Buffer
	b0 := opcode & 0x0F
	if fin {
		b0 |= 0x80
	}
	out.WriteByte(b0)
	n := len(payload)
	switch {
	case n < 126:
		out.WriteByte(0x80 | byte(n))
	case n <= 65535:
		out.WriteByte(0x80 | 126)
		var ext [2]byte
		binary.BigEndian.PutUint16(ext[:], uint16(n))
		out.Write(ext[:])
	default:
		out.WriteByte(0x80 | 127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		out.Write(ext[:])
	}
	out.Write(mask[:])
	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	out.Write(masked)
	return out.Bytes()
}

// FuzzWebSocketFrames fuzzes RFC 6455 WebSocket frame parsing (wsSubscriberConn.readMessage)
// and frame serialization (writeFrame / writeClose) across masked, fragmented, extended-length,
// interleaved control (Ping/Pong/Close), and malformed frames to verify zero panics or OOMs.
func FuzzWebSocketFrames(f *testing.F) {
	mask := [4]byte{0x37, 0xFA, 0x21, 0x3D}
	seeds := [][]byte{
		buildMaskedClientWSFrame(true, wsOpText, mask, []byte("bind: wsbt-12345")),
		buildMaskedClientWSFrame(true, wsOpText, mask, []byte("ping")),
		buildMaskedClientWSFrame(true, wsOpBinary, mask, bytes.Repeat([]byte{0xAB}, 300)),
		append(
			append(
				buildMaskedClientWSFrame(false, wsOpText, mask, []byte("bind-with-")),
				buildMaskedClientWSFrame(true, wsOpPing, mask, []byte("hb"))...,
			),
			buildMaskedClientWSFrame(true, wsOpContinuation, mask, []byte("token: tok-1"))...,
		),
		buildMaskedClientWSFrame(true, wsOpClose, mask, []byte{0x03, 0xE8, 'b', 'y', 'e'}),
		// Unmasked client frame (must fail cleanly with protocol error 1002)
		{0x81, 0x04, 'p', 'i', 'n', 'g'},
		// Non-zero RSV bits (must fail cleanly with protocol error 1002)
		{0xF1, 0x80, 0, 0, 0, 0},
		// 64-bit length overflow / oversized payload header (must fail cleanly with 1009, never OOM)
		{0x82, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 1, 2, 3, 4},
		// Unexpected continuation frame
		buildMaskedClientWSFrame(true, wsOpContinuation, mask, []byte("orphan")),
		// Truncated header
		{0x81},
		{},
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, wire []byte) {
		fc := &fuzzWSNetConn{reader: bytes.NewReader(wire)}
		ws := &wsSubscriberConn{
			id:     "fuzz-ws",
			tenant: "default",
			conn:   fc,
			bufrw:  bufio.NewReadWriter(bufio.NewReader(fc), bufio.NewWriter(io.Discard)),
			done:   make(chan struct{}),
		}
		for range 8 {
			_, _, err := ws.readMessage()
			if err != nil {
				break
			}
		}

		// Also verify valid masked client frame round-trip for bounded payloads
		if len(wire) <= 4096 {
			validWire := buildMaskedClientWSFrame(true, wsOpBinary, mask, wire)
			rtConn := &fuzzWSNetConn{reader: bytes.NewReader(validWire)}
			rtWS := &wsSubscriberConn{
				id:     "fuzz-ws-rt",
				tenant: "default",
				conn:   rtConn,
				bufrw:  bufio.NewReadWriter(bufio.NewReader(rtConn), bufio.NewWriter(io.Discard)),
				done:   make(chan struct{}),
			}
			op, got, err := rtWS.readMessage()
			if err != nil {
				t.Fatalf("unexpected error decoding valid masked frame (len=%d): %v", len(wire), err)
			}
			if op != wsOpBinary || !bytes.Equal(got, wire) {
				t.Fatalf("round-trip mismatch: got op=%d len=%d, want op=%d len=%d", op, len(got), wsOpBinary, len(wire))
			}
			_ = rtWS.writeFrame(wsOpBinary, wire)
			rtWS.writeClose(1000, string(wire))
		}
	})
}
