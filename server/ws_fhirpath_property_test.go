package server

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"reflect"
	"sort"
	"testing"
	"time"

	"hegel.dev/go/hegel"
)

func buildMaskedClientFrame(fin bool, opcode byte, maskKey [4]byte, payload []byte) []byte {
	var buf bytes.Buffer
	b0 := opcode & 0x0F
	if fin {
		b0 |= 0x80
	}
	buf.WriteByte(b0)

	n := len(payload)
	switch {
	case n < 126:
		buf.WriteByte(0x80 | byte(n))
	case n <= 65535:
		buf.WriteByte(0x80 | 126)
		var ext [2]byte
		binary.BigEndian.PutUint16(ext[:], uint16(n))
		buf.Write(ext[:])
	default:
		buf.WriteByte(0x80 | 127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		buf.Write(ext[:])
	}

	buf.Write(maskKey[:])
	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ maskKey[i%4]
	}
	buf.Write(masked)
	return buf.Bytes()
}

func readServerUnmaskedFrame(r io.Reader) (fin bool, opcode byte, payload []byte, err error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return false, 0, nil, err
	}
	fin = (hdr[0] & 0x80) != 0
	if (hdr[0] & 0x70) != 0 {
		return false, 0, nil, fmt.Errorf("unexpected server RSV bits: 0x%x", hdr[0])
	}
	opcode = hdr[0] & 0x0F
	if (hdr[1] & 0x80) != 0 {
		return false, 0, nil, fmt.Errorf("server frames must not be masked per RFC 6455 §5.1")
	}
	length := uint64(hdr[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	payload = make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return false, 0, nil, err
		}
	}
	return fin, opcode, payload, nil
}

// TestProperty_WebSocketRFC6455RoundTripAndFragmentation verifies RFC 6455 frame encoding
// and decoding invariants across all 3 payload length encodings (7-bit, 16-bit, 64-bit),
// arbitrary 4-byte masking keys, multi-frame continuation fragmentation, mid-stream Ping->Pong
// control frame interleaving, and 1002/1009 protocol violation close codes.
func TestProperty_WebSocketRFC6455RoundTripAndFragmentation(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		sizeClass := hegel.Draw(ht, hegel.SampledFrom([]string{"tiny_7bit", "medium_16bit", "large_64bit_server"}))
		opcode := hegel.Draw(ht, hegel.SampledFrom([]byte{wsOpText, wsOpBinary}))
		maskBytes := hegel.Draw(ht, hegel.Binary(4, 4))
		var maskKey [4]byte
		copy(maskKey[:], maskBytes)

		var payloadLen int
		switch sizeClass {
		case "tiny_7bit":
			payloadLen = hegel.Draw(ht, hegel.Integers(0, 125))
		case "medium_16bit":
			payloadLen = hegel.Draw(ht, hegel.Integers(126, 8192))
		case "large_64bit_server":
			payloadLen = hegel.Draw(ht, hegel.Integers(65536, 67000))
		}

		seedByte := byte(hegel.Draw(ht, hegel.Integers(1, 254)))
		payload := make([]byte, payloadLen)
		for i := range payload {
			payload[i] = byte((int(seedByte)*31 + i*17) & 0x7F)
		}

		srvSide, cliSide := net.Pipe()
		_ = srvSide.SetDeadline(time.Now().Add(5 * time.Second))
		_ = cliSide.SetDeadline(time.Now().Add(5 * time.Second))
		defer srvSide.Close()
		defer cliSide.Close()

		wsConn := &wsSubscriberConn{
			id:     "pbt-ws",
			tenant: "default",
			conn:   srvSide,
			bufrw:  bufio.NewReadWriter(bufio.NewReader(srvSide), bufio.NewWriter(srvSide)),
			done:   make(chan struct{}),
		}

		// 1. Server -> Client writeFrame across all 3 length classes (including >65535 64-bit header!)
		errCh := make(chan error, 1)
		go func() {
			errCh <- wsConn.writeFrame(opcode, payload)
		}()

		fin, gotOp, gotPayload, err := readServerUnmaskedFrame(cliSide)
		if err != nil {
			ht.Fatalf("readServerUnmaskedFrame(%s, len=%d) failed: %v", sizeClass, payloadLen, err)
		}
		if werr := <-errCh; werr != nil {
			ht.Fatalf("writeFrame(%s, len=%d) failed: %v", sizeClass, payloadLen, werr)
		}
		if !fin || gotOp != opcode || !bytes.Equal(gotPayload, payload) {
			ht.Fatalf("writeFrame mismatch: fin=%v op=%d len=%d (want len=%d)", fin, gotOp, len(gotPayload), len(payload))
		}

		// 2. Client -> Server masked + fragmented + interleaved Ping/Pong round-trip!
		if payloadLen <= maxWSMessageBytes {
			fragCount := hegel.Draw(ht, hegel.Integers(1, 4))
			interleavePing := hegel.Draw(ht, hegel.Booleans())
			pingPayload := hegel.Draw(ht, hegel.Binary(0, 32))

			var wireStream bytes.Buffer
			pingInjected := false
			if fragCount == 1 || payloadLen <= 1 {
				if interleavePing {
					wireStream.Write(buildMaskedClientFrame(true, wsOpPing, maskKey, pingPayload))
					pingInjected = true
				}
				wireStream.Write(buildMaskedClientFrame(true, opcode, maskKey, payload))
			} else {
				chunkSize := max(payloadLen/fragCount, 1)
				offset := 0
				for f := 0; f < fragCount; f++ {
					end := offset + chunkSize
					isLast := f == fragCount-1 || end >= payloadLen
					if isLast {
						end = payloadLen
					}
					frameOp := byte(wsOpContinuation)
					if f == 0 {
						frameOp = opcode
					}
					var fMask [4]byte
					for b := range 4 {
						fMask[b] = maskKey[b] ^ byte(f+1)
					}
					wireStream.Write(buildMaskedClientFrame(isLast, frameOp, fMask, payload[offset:end]))
					if f == 0 && interleavePing && !isLast {
						// RFC 6455 §5.5: Control frame injected mid-fragmentation!
						wireStream.Write(buildMaskedClientFrame(true, wsOpPing, maskKey, pingPayload))
						pingInjected = true
					}
					offset = end
					if isLast {
						break
					}
				}
			}

			type readRes struct {
				op   byte
				data []byte
				err  error
			}
			readCh := make(chan readRes, 1)
			go func() {
				ro, rd, rerr := wsConn.readMessage()
				readCh <- readRes{op: ro, data: rd, err: rerr}
			}()

			// Drain expected Pong concurrently if Ping was interleaved so net.Pipe doesn't block!
			pongErrCh := make(chan error, 1)
			if pingInjected {
				go func() {
					pFin, pOp, pData, pErr := readServerUnmaskedFrame(cliSide)
					if pErr != nil {
						pongErrCh <- pErr
						return
					}
					if !pFin || pOp != wsOpPong || !bytes.Equal(pData, pingPayload) {
						pongErrCh <- fmt.Errorf("pong mismatch: fin=%v op=0x%x got=%x want=%x", pFin, pOp, pData, pingPayload)
						return
					}
					pongErrCh <- nil
				}()
			}

			if _, err := cliSide.Write(wireStream.Bytes()); err != nil {
				ht.Fatalf("client write fragmented stream: %v", err)
			}
			if pingInjected {
				if pErr := <-pongErrCh; pErr != nil {
					ht.Fatalf("interleaved Ping->Pong failed: %v", pErr)
				}
			}
			rr := <-readCh
			if rr.err != nil {
				ht.Fatalf("readMessage(len=%d, frags=%d) failed: %v", payloadLen, fragCount, rr.err)
			}
			if rr.op != opcode || !bytes.Equal(rr.data, payload) {
				ht.Fatalf("readMessage reassembly mismatch: op=0x%x gotLen=%d wantLen=%d", rr.op, len(rr.data), len(payload))
			}
		} else {
			// > maxWSMessageBytes (65536) from client MUST be rejected with Close 1009 (Message Too Big)!
			oversizedFrame := buildMaskedClientFrame(true, opcode, maskKey, payload)
			closeCodeCh := make(chan uint16, 1)
			go func() {
				_, cOp, cData, cErr := readServerUnmaskedFrame(cliSide)
				if cErr == nil && cOp == wsOpClose && len(cData) >= 2 {
					closeCodeCh <- binary.BigEndian.Uint16(cData[:2])
					return
				}
				closeCodeCh <- 0
			}()
			go func() {
				_, _ = cliSide.Write(oversizedFrame)
			}()
			_, _, rerr := wsConn.readMessage()
			if rerr == nil {
				ht.Fatalf("expected oversized client frame (%d bytes) to return error", payloadLen)
			}
			if code := <-closeCodeCh; code != 1009 {
				ht.Fatalf("expected RFC 6455 close status 1009 for oversized frame, got %d", code)
			}
		}
	}, hegel.WithTestCases(80))
}

// TestProperty_FHIRPathKleeneThreeValuedAndCollectionLaws verifies HL7 FHIRPath R4 §6.5
// three-valued Kleene logic laws (commutativity, associativity, dominance/absorption with {},
// De Morgan's laws, implication identity) and collection laws (distinct idempotence, union symmetry).
func TestProperty_FHIRPathKleeneThreeValuedAndCollectionLaws(t *testing.T) {
	atoms := []string{"true", "false", "{}"}

	hegel.Test(t, func(ht *hegel.T) {
		a := hegel.Draw(ht, hegel.SampledFrom(atoms))
		b := hegel.Draw(ht, hegel.SampledFrom(atoms))
		c := hegel.Draw(ht, hegel.SampledFrom(atoms))

		doc := map[string]any{"resourceType": "Patient", "id": "fp-law"}

		eval := func(expr string) []any {
			res := evalFHIRPath(doc, expr)
			if len(res) == 0 {
				return nil
			}
			return res
		}

		// 1. Commutativity of and, or, xor
		if !reflect.DeepEqual(eval(fmt.Sprintf("(%s) and (%s)", a, b)), eval(fmt.Sprintf("(%s) and (%s)", b, a))) {
			ht.Fatalf("Kleene AND commutativity violated for (%s, %s)", a, b)
		}
		if !reflect.DeepEqual(eval(fmt.Sprintf("(%s) or (%s)", a, b)), eval(fmt.Sprintf("(%s) or (%s)", b, a))) {
			ht.Fatalf("Kleene OR commutativity violated for (%s, %s)", a, b)
		}
		if !reflect.DeepEqual(eval(fmt.Sprintf("(%s) xor (%s)", a, b)), eval(fmt.Sprintf("(%s) xor (%s)", b, a))) {
			ht.Fatalf("Kleene XOR commutativity violated for (%s, %s)", a, b)
		}

		// 2. Associativity of and, or
		lhsAnd := eval(fmt.Sprintf("((%s) and (%s)) and (%s)", a, b, c))
		rhsAnd := eval(fmt.Sprintf("(%s) and ((%s) and (%s))", a, b, c))
		if !reflect.DeepEqual(lhsAnd, rhsAnd) {
			ht.Fatalf("Kleene AND associativity violated for (%s, %s, %s): %v != %v", a, b, c, lhsAnd, rhsAnd)
		}
		lhsOr := eval(fmt.Sprintf("((%s) or (%s)) or (%s)", a, b, c))
		rhsOr := eval(fmt.Sprintf("(%s) or ((%s) or (%s))", a, b, c))
		if !reflect.DeepEqual(lhsOr, rhsOr) {
			ht.Fatalf("Kleene OR associativity violated for (%s, %s, %s): %v != %v", a, b, c, lhsOr, rhsOr)
		}

		// 3. De Morgan's Laws across 3-valued {true, false, {}}!
		dmAndLHS := eval(fmt.Sprintf("((%s) and (%s)).not()", a, b))
		dmAndRHS := eval(fmt.Sprintf("(%s).not() or (%s).not()", a, b))
		if !reflect.DeepEqual(dmAndLHS, dmAndRHS) {
			ht.Fatalf("Kleene De Morgan NOT(A and B) violated for (%s, %s): %v != %v", a, b, dmAndLHS, dmAndRHS)
		}
		dmOrLHS := eval(fmt.Sprintf("((%s) or (%s)).not()", a, b))
		dmOrRHS := eval(fmt.Sprintf("(%s).not() and (%s).not()", a, b))
		if !reflect.DeepEqual(dmOrLHS, dmOrRHS) {
			ht.Fatalf("Kleene De Morgan NOT(A or B) violated for (%s, %s): %v != %v", a, b, dmOrLHS, dmOrRHS)
		}

		// 4. Kleene Implication Identity: (A implies B) == (A.not() or B)
		impLHS := eval(fmt.Sprintf("(%s) implies (%s)", a, b))
		impRHS := eval(fmt.Sprintf("(%s).not() or (%s)", a, b))
		if !reflect.DeepEqual(impLHS, impRHS) {
			ht.Fatalf("Kleene implication identity (A implies B) == (A.not() or B) violated for (%s, %s): %v != %v", a, b, impLHS, impRHS)
		}

		// 5. Dominance laws with {}
		if !reflect.DeepEqual(eval("false and {}"), []any{false}) {
			ht.Fatalf("expected (false and {}) == [false], got %v", eval("false and {}"))
		}
		if !reflect.DeepEqual(eval("true or {}"), []any{true}) {
			ht.Fatalf("expected (true or {}) == [true], got %v", eval("true or {}"))
		}
		if eval("true and {}") != nil {
			ht.Fatalf("expected (true and {}) == [], got %v", eval("true and {}"))
		}
		if eval("false or {}") != nil {
			ht.Fatalf("expected (false or {}) == [], got %v", eval("false or {}"))
		}

		// 6. Collection algebraic laws over randomized Patient.name.given lists
		g1 := hegel.Draw(ht, hegel.Lists(hegel.SampledFrom([]string{"Ada", "Grace", "Alan", "Marie"})).MinSize(0).MaxSize(5))
		g2 := hegel.Draw(ht, hegel.Lists(hegel.SampledFrom([]string{"Grace", "Linus", "Ada", "Ken"})).MinSize(0).MaxSize(5))
		toAny := func(ss []string) []any {
			out := make([]any, len(ss))
			for i, s := range ss {
				out[i] = s
			}
			return out
		}
		colDoc := map[string]any{
			"resourceType": "Patient",
			"name": []any{
				map[string]any{"use": "official", "given": toAny(g1)},
				map[string]any{"use": "maiden", "given": toAny(g2)},
			},
		}

		d1 := evalFHIRPath(colDoc, "name.given.distinct()")
		d2 := evalFHIRPath(colDoc, "name.given.distinct().distinct()")
		if !reflect.DeepEqual(d1, d2) {
			ht.Fatalf("distinct() idempotence violated: %v != %v", d1, d2)
		}

		// Union set-equality symmetry & idempotence: (A | B) has same distinct set as (B | A)
		uAB := evalFHIRPath(colDoc, "name.where(use = 'official').given | name.where(use = 'maiden').given")
		uBA := evalFHIRPath(colDoc, "name.where(use = 'maiden').given | name.where(use = 'official').given")
		toSortedStrings := func(in []any) []string {
			out := make([]string, 0, len(in))
			for _, v := range in {
				out = append(out, fmt.Sprint(v))
			}
			sort.Strings(out)
			return out
		}
		if !reflect.DeepEqual(toSortedStrings(uAB), toSortedStrings(uBA)) {
			ht.Fatalf("FHIRPath pipe union set symmetry violated: %v vs %v", uAB, uBA)
		}

		cnt := evalFHIRPath(colDoc, "name.given.count()")
		isEmpty := evalFHIRPath(colDoc, "name.given.empty()")
		exists := evalFHIRPath(colDoc, "name.given.exists()")
		totalItems := len(g1) + len(g2)
		if len(cnt) != 1 || fmt.Sprint(cnt[0]) != fmt.Sprint(totalItems) {
			ht.Fatalf("count() mismatch: got %v, want [%d]", cnt, totalItems)
		}
		if len(isEmpty) != 1 || isEmpty[0] != (totalItems == 0) {
			ht.Fatalf("empty() mismatch: got %v, want [%v]", isEmpty, totalItems == 0)
		}
		if len(exists) != 1 || exists[0] != (totalItems > 0) {
			ht.Fatalf("exists() mismatch: got %v, want [%v]", exists, totalItems > 0)
		}
	}, hegel.WithTestCases(150))
}

// TestProperty_PatchPureEnginesAtomicityAndInputImmutability verifies that ApplyJSONPatch,
// ApplyFHIRPatch, and ApplyMergePatch never mutate caller input buffers and abort cleanly
// on failed assertions or invalid paths.
func TestProperty_PatchPureEnginesAtomicityAndInputImmutability(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		family := hegel.Draw(ht, hegel.FromRegex(`[A-Z][a-z]{3,9}`, true))
		newFamily := hegel.Draw(ht, hegel.FromRegex(`[A-Z][a-z]{3,9}`, true))
		gender := hegel.Draw(ht, hegel.SampledFrom([]string{"male", "female", "other"}))

		origBytes := []byte(fmt.Sprintf(`{"resourceType":"Patient","id":"pat-1","active":true,"gender":%q,"name":[{"family":%q,"given":["Test"]}]}`, gender, family))
		origCopy := append([]byte(nil), origBytes...)

		// 1. Multi-op JSON Patch ending in a failing `test` op MUST fail atomically!
		failingJSONPatch := []byte(fmt.Sprintf(`[
			{"op":"replace","path":"/name/0/family","value":%q},
			{"op":"replace","path":"/active","value":false},
			{"op":"test","path":"/gender","value":"impossible-gender"}
		]`, newFamily))
		_, status, err := ApplyJSONPatch(origBytes, failingJSONPatch)
		if err == nil || status < 400 {
			ht.Fatalf("expected failing RFC 6902 test op to fail with >=400, got status=%d err=%v", status, err)
		}
		if !bytes.Equal(origBytes, origCopy) {
			ht.Fatalf("ApplyJSONPatch mutated caller input slice!")
		}

		// 2. Valid JSON Patch succeeds and preserves untouched fields.
		validJSONPatch := []byte(fmt.Sprintf(`[
			{"op":"test","path":"/gender","value":%q},
			{"op":"replace","path":"/name/0/family","value":%q}
		]`, gender, newFamily))
		patched, status, err := ApplyJSONPatch(origBytes, validJSONPatch)
		if err != nil || status != 200 {
			ht.Fatalf("valid ApplyJSONPatch failed: status=%d err=%v", status, err)
		}
		if !bytes.Equal(origBytes, origCopy) {
			ht.Fatalf("valid ApplyJSONPatch mutated caller input slice!")
		}
		var m map[string]any
		if uerr := json.Unmarshal(patched, &m); uerr != nil {
			ht.Fatalf("unmarshal patched: %v", uerr)
		}
		if m["gender"] != gender || m["active"] != true {
			ht.Fatalf("untouched fields corrupted after ApplyJSONPatch: %v", m)
		}

		// 3. RFC 7396 Merge Patch deletes null keys and preserves caller input slice.
		mergeBody := []byte(fmt.Sprintf(`{"active":false,"gender":null,"telecom":[{"system":"phone","value":%q}]}`, family))
		merged, mErr := ApplyMergePatch(origBytes, mergeBody)
		if mErr != nil {
			ht.Fatalf("valid ApplyMergePatch failed: err=%v", mErr)
		}
		if !bytes.Equal(origBytes, origCopy) {
			ht.Fatalf("ApplyMergePatch mutated caller input slice!")
		}
		var mm map[string]any
		if uerr := json.Unmarshal(merged, &mm); uerr != nil {
			ht.Fatalf("unmarshal merged: %v", uerr)
		}
		if _, hasGender := mm["gender"]; hasGender || mm["active"] != false {
			ht.Fatalf("ApplyMergePatch null deletion or update failed: %v", mm)
		}
	}, hegel.WithTestCases(100))
}
