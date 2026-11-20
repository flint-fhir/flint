package server_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/pkg/audit"
	"github.com/flint-fhir/flint/pkg/ccda"
	"github.com/flint-fhir/flint/pkg/hl7v2"
	"github.com/flint-fhir/flint/pkg/telemetry"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/sqlite"
)

func newTrack4TestServer(t *testing.T, name string) (*server.Server, *httptest.Server, *sqlite.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), name+".db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	require.NoError(t, sqlite.RunMigrations(t.Context(), db, sqlite.JournalWAL))

	st := sqlite.New(db, sqlite.WithJournalMode(sqlite.JournalWAL), sqlite.WithDSN(dbPath))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(st, logger)
	server.RegisterDefaultResourceTypes(srv)
	srv.SetAuditRecorder(audit.NewStoreRecorder(st))
	t.Cleanup(func() { _ = srv.Close() })

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts, st
}

func doTrack4HTTP(
	t *testing.T,
	client *http.Client,
	method, targetURL, contentType, accept string,
	body []byte,
	extraHeaders map[string]string,
) (int, http.Header, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, targetURL, rdr)
	require.NoError(t, err)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header, raw
}

func readTrack4Vector(t *testing.T, relPath string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", relPath))
	require.NoError(t, err)
	return b
}

func TestHL7v2_HTTPIngestAndMLLPLiveRoundTrip(t *testing.T) {
	srv, ts, _ := newTrack4TestServer(t, "hl7v2-e2e")
	tenant := "t4-hl7"
	base := ts.URL + "/fhir/r4/" + tenant

	var spanMu sync.Mutex
	var spans []telemetry.SpanData
	tr, err := telemetry.NewTracer(telemetry.Config{
		Endpoint:       "http://127.0.0.1:4318",
		Protocol:       "http/protobuf",
		ServiceName:    "flint-t4-test",
		ServiceVersion: "0.7.0",
		BatchSize:      1,
		FlushInterval:  10 * time.Millisecond,
		Exporter: func(_ context.Context, batch []telemetry.SpanData) error {
			spanMu.Lock()
			spans = append(spans, batch...)
			spanMu.Unlock()
			return nil
		},
	})
	require.NoError(t, err)
	srv.SetTracer(tr)

	adtBytes := readTrack4Vector(t, "pkg/hl7v2/testdata/nist_adt_a01.hl7")
	oruBytes := readTrack4Vector(t, "pkg/hl7v2/testdata/nist_oru_r01_lri.hl7")
	vxuBytes := readTrack4Vector(t, "pkg/hl7v2/testdata/nist_vxu_v04_iz.hl7")
	ormBytes := readTrack4Vector(t, "pkg/hl7v2/testdata/nist_orm_o01.hl7")

	// 1. Raw ER7 HTTP ingestion across ADT^A01, ORU^R01, VXU^V04, ORM^O01 + idempotent replay
	var adtPatientRef string
	for idx, vec := range [][]byte{adtBytes, adtBytes, oruBytes, vxuBytes, ormBytes} {
		status, _, body := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/$ingest-hl7v2", "x-application/hl7-v2+er7", "application/fhir+json", vec, nil)
		require.Equal(t, http.StatusOK, status, "vector %d body=%s", idx, string(body))

		var params map[string]any
		require.NoError(t, json.Unmarshal(body, &params))
		assert.Equal(t, "Parameters", params["resourceType"])

		var ackCode, ackStr, patRef string
		for _, item := range params["parameter"].([]any) {
			pm := item.(map[string]any)
			switch pm["name"] {
			case "ackCode":
				ackCode, _ = pm["valueCode"].(string)
			case "ack":
				ackStr, _ = pm["valueString"].(string)
			case "patient":
				patRef, _ = pm["valueReference"].(map[string]any)["reference"].(string)
			}
		}
		assert.Equal(t, "AA", ackCode)
		assert.Contains(t, ackStr, "MSA|AA|")
		require.True(t, strings.HasPrefix(patRef, "Patient/"))
		if idx == 0 {
			adtPatientRef = patRef
		}
		if idx == 1 {
			assert.Equal(t, adtPatientRef, patRef, "duplicate ADT^A01 must resolve to identical Patient ID")
		}
	}

	// Verify duplicate ADT^A01 did not create duplicate Patient records
	stPat, _, patSearch := doTrack4HTTP(t, ts.Client(), http.MethodGet, base+"/Patient?_id="+strings.TrimPrefix(adtPatientRef, "Patient/"), "", "application/fhir+json", nil, nil)
	require.Equal(t, http.StatusOK, stPat)
	var patBundle map[string]any
	require.NoError(t, json.Unmarshal(patSearch, &patBundle))
	assert.Equal(t, float64(1), patBundle["total"])

	// Verify mapped Immunization (VXU^V04), ServiceRequest (ORM^O01), and DiagnosticReport (ORU^R01)
	for _, rt := range []string{"Immunization", "ServiceRequest", "DiagnosticReport", "Observation", "Provenance"} {
		stRes, _, resBody := doTrack4HTTP(t, ts.Client(), http.MethodGet, base+"/"+rt+"?_count=10", "", "application/fhir+json", nil, nil)
		require.Equal(t, http.StatusOK, stRes)
		var b map[string]any
		require.NoError(t, json.Unmarshal(resBody, &b))
		assert.GreaterOrEqual(t, b["total"], float64(1), "expected %s resources from HL7 v2 vectors", rt)
	}

	// 2. Accept: x-application/hl7-v2+er7 returns raw ER7 ACK with wire format header
	stER7, hdrER7, bodyER7 := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/$ingest-hl7v2", "x-application/hl7-v2+er7", "x-application/hl7-v2+er7", adtBytes, nil)
	require.Equal(t, http.StatusOK, stER7)
	assert.Contains(t, strings.ToLower(hdrER7.Get("Content-Type")), "x-application/hl7-v2+er7")
	assert.Equal(t, "hl7.v2.er7", hdrER7.Get("X-Flint-Wire-Format"))
	assert.Contains(t, string(bodyER7), "MSA|AA|")

	// 3. FHIR Parameters wire parity: JSON, FHIR XML, and Binary Proto3
	paramsJSON, err := json.Marshal(map[string]any{
		"resourceType": "Parameters",
		"parameter": []map[string]any{
			{"name": "message", "valueString": string(adtBytes)},
		},
	})
	require.NoError(t, err)

	stPJSON, _, bPJSON := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/$ingest-hl7v2", "application/fhir+json", "application/fhir+json", paramsJSON, nil)
	require.Equal(t, http.StatusOK, stPJSON, string(bPJSON))
	assert.Contains(t, string(bPJSON), `"valueCode":"AA"`)

	stConvXML, _, paramsXML := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/Parameters/$convert", "application/fhir+json", "application/fhir+xml", paramsJSON, nil)
	require.Equal(t, http.StatusOK, stConvXML)
	stPXML, hdrPXML, bPXML := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/$ingest-hl7v2", "application/fhir+xml", "application/fhir+xml", paramsXML, nil)
	require.Equal(t, http.StatusOK, stPXML, string(bPXML))
	assert.Contains(t, strings.ToLower(hdrPXML.Get("Content-Type")), "application/fhir+xml")
	assert.Contains(t, string(bPXML), `<valueCode value="AA"/>`)

	stConvPB, _, paramsPB := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/Parameters/$convert", "application/fhir+json", "application/x-protobuf", paramsJSON, nil)
	require.Equal(t, http.StatusOK, stConvPB)
	stPPB, hdrPPB, bPPB := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/$ingest-hl7v2", "application/x-protobuf", "application/x-protobuf", paramsPB, nil)
	require.Equal(t, http.StatusOK, stPPB, string(bPPB))
	assert.Contains(t, strings.ToLower(hdrPPB.Get("Content-Type")), "application/x-protobuf")
	assert.NotEmpty(t, bPPB)

	// 4. Error branches: malformed ER7, unsupported message type QBP^Q11, empty body, and oversized payload
	stBadER7, _, bBadER7 := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/$ingest-hl7v2", "x-application/hl7-v2+er7", "x-application/hl7-v2+er7", []byte("INVALID-NO-MSH\r"), nil)
	require.Equal(t, http.StatusBadRequest, stBadER7)
	assert.Contains(t, string(bBadER7), "MSA|AR|")

	stBadJSON, _, bBadJSON := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/$ingest-hl7v2", "x-application/hl7-v2+er7", "application/fhir+json", []byte("INVALID-NO-MSH\r"), nil)
	require.Equal(t, http.StatusBadRequest, stBadJSON)
	assert.Contains(t, string(bBadJSON), "OperationOutcome")
	assert.Contains(t, string(bBadJSON), "MSA|AR|")

	unsupMsg := []byte("MSH|^~\\&|APP|FAC|FLINT|HOSP|20261119120000Z||QBP^Q11^QBP_Q11|UNSUP-1|P|2.5.1\rPID|1||MRN-U1\r")
	stUnsup, _, bUnsup := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/$ingest-hl7v2", "x-application/hl7-v2+er7", "x-application/hl7-v2+er7", unsupMsg, nil)
	require.Equal(t, http.StatusBadRequest, stUnsup)
	assert.Contains(t, string(bUnsup), "MSA|AR|UNSUP-1")

	oversized := bytes.Repeat([]byte("A"), hl7v2.MaxMessageBytes+64)
	stBig, _, _ := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/$ingest-hl7v2", "x-application/hl7-v2+er7", "application/fhir+json", oversized, nil)
	require.Equal(t, http.StatusRequestEntityTooLarge, stBig)

	// 5. Live MLLP TCP server round-trip + AuditEvent + OTel span + Prometheus counters
	mllpSrv := srv.NewMLLPServer("127.0.0.1:0", tenant)
	require.NoError(t, mllpSrv.Start())
	defer func() { _ = mllpSrv.Close() }()

	conn, err := net.Dial("tcp", mllpSrv.Addr())
	require.NoError(t, err)
	defer conn.Close()
	rd := bufio.NewReader(conn)

	mllpADT := []byte("MSH|^~\\&|EHR|MLLP_FAC|FLINT|HOSP|20261119130000+0000||ADT^A01^ADT_A01|MLLP-LIVE-1|P|2.5.1\rPID|1||MLLP-MRN-99^^^MLLP_FAC^MR||Mllp^Patient||19790520|M\rPV1|1|I|ICU^202^B\r")
	require.NoError(t, hl7v2.WriteMLLPFrame(conn, mllpADT))
	ack1, err := hl7v2.ReadMLLPFrame(rd, hl7v2.MaxMessageBytes)
	require.NoError(t, err)
	assert.Contains(t, string(ack1), "MSA|AA|MLLP-LIVE-1")

	mllpORU := []byte("MSH|^~\\&|LAB|MLLP_FAC|FLINT|HOSP|20261119130500+0000||ORU^R01^ORU_R01|MLLP-LIVE-2|P|2.5.1\rPID|1||MLLP-MRN-99^^^MLLP_FAC^MR||Mllp^Patient||19790520|M\rOBR|1|MORD1|MFILL1|8867-4^Heart rate^LN|||20261119130000+0000|||||||||||||||20261119130500+0000|||F\rOBX|1|NM|8867-4^Heart rate^LN||68|/min|60-100|N|||F|||20261119130000+0000\r")
	require.NoError(t, hl7v2.WriteMLLPFrame(conn, mllpORU))
	ack2, err := hl7v2.ReadMLLPFrame(rd, hl7v2.MaxMessageBytes)
	require.NoError(t, err)
	assert.Contains(t, string(ack2), "MSA|AA|MLLP-LIVE-2")

	// Malformed + unsupported message over MLLP -> AR NACK and span records hl7v2.message_type
	require.NoError(t, hl7v2.WriteMLLPFrame(conn, []byte("BAD-MLLP-FRAME\r")))
	nackBad, err := hl7v2.ReadMLLPFrame(rd, hl7v2.MaxMessageBytes)
	require.NoError(t, err)
	assert.Contains(t, string(nackBad), "MSA|AR|")

	mllpUnsup := []byte("MSH|^~\\&|LAB|MLLP_FAC|FLINT|HOSP|20261119130600+0000||QBP^Q11^QBP_Q11|MLLP-UNSUP-3|P|2.5.1\rPID|1||MLLP-MRN-99^^^MLLP_FAC^MR\r")
	require.NoError(t, hl7v2.WriteMLLPFrame(conn, mllpUnsup))
	nackUnsup, err := hl7v2.ReadMLLPFrame(rd, hl7v2.MaxMessageBytes)
	require.NoError(t, err)
	assert.Contains(t, string(nackUnsup), "MSA|AR|MLLP-UNSUP-3")

	// Wait briefly for async OTLP batch worker (BatchSize=1) and verify MLLP INGEST spans
	var foundSuccessSpan, foundErrorMsgTypeSpan bool
	require.Eventually(t, func() bool {
		spanMu.Lock()
		defer spanMu.Unlock()
		foundSuccessSpan = false
		foundErrorMsgTypeSpan = false
		for _, sp := range spans {
			if sp.Name != "MLLP INGEST" {
				continue
			}
			mtAttr := ""
			for _, attr := range sp.Attributes {
				if attr.Key == "hl7v2.message_type" {
					mtAttr = attr.StrVal
				}
			}
			if mtAttr == "ADT^A01" && sp.Status == telemetry.StatusOK {
				foundSuccessSpan = true
			}
			if mtAttr == "QBP^Q11" && sp.Status == telemetry.StatusError {
				foundErrorMsgTypeSpan = true
			}
		}
		return foundSuccessSpan && foundErrorMsgTypeSpan
	}, 2*time.Second, 10*time.Millisecond, "expected OTLP MLLP INGEST spans for ADT^A01 and QBP^Q11")
	assert.True(t, foundSuccessSpan, "expected OTel MLLP INGEST span for ADT^A01")
	assert.True(t, foundErrorMsgTypeSpan, "expected OTel MLLP INGEST error span with hl7v2.message_type=QBP^Q11")

	// Verify MLLP AuditEvent recorded
	stAud, _, audBody := doTrack4HTTP(t, ts.Client(), http.MethodGet, base+"/AuditEvent?subtype=ingest-hl7v2-mllp&_count=10", "", "application/fhir+json", nil, nil)
	require.Equal(t, http.StatusOK, stAud)
	assert.Contains(t, string(audBody), `"ingest-hl7v2-mllp"`)

	// Verify Prometheus metrics counters
	stMet, _, metBody := doTrack4HTTP(t, ts.Client(), http.MethodGet, ts.URL+"/metrics", "", "text/plain", nil, nil)
	require.Equal(t, http.StatusOK, stMet)
	metStr := string(metBody)
	assert.Contains(t, metStr, "flint_hl7v2_messages_ingested_total ")
	assert.NotContains(t, metStr, "flint_hl7v2_messages_ingested_total 0\n")
	assert.Contains(t, metStr, "flint_hl7v2_mllp_messages_total 2\n")
	assert.Contains(t, metStr, "flint_hl7v2_messages_errors_total ")
	assert.NotContains(t, metStr, "flint_hl7v2_messages_errors_total 0\n")

	// 6. Enable strict SMART token enforcement on HTTP and confirm trusted MLLP listener STILL succeeds
	srv.EnableDevTokenEnforcement()

	stUnauthHTTP, _, _ := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/$ingest-hl7v2", "x-application/hl7-v2+er7", "application/fhir+json", adtBytes, nil)
	assert.Equal(t, http.StatusUnauthorized, stUnauthHTTP)

	mllpAuthedTest := []byte("MSH|^~\\&|EHR|MLLP_FAC|FLINT|HOSP|20261119131000+0000||ADT^A08^ADT_A08|MLLP-BYPASS-4|P|2.5.1\rPID|1||MLLP-MRN-99^^^MLLP_FAC^MR||Mllp^Patient||19790520|M\r")
	require.NoError(t, hl7v2.WriteMLLPFrame(conn, mllpAuthedTest))
	ackBypass, err := hl7v2.ReadMLLPFrame(rd, hl7v2.MaxMessageBytes)
	require.NoError(t, err)
	assert.Contains(t, string(ackBypass), "MSA|AA|MLLP-BYPASS-4")
}

func TestCCDA_BidirectionalImportExportAndSecurity(t *testing.T) {
	_, ts, _ := newTrack4TestServer(t, "ccda-e2e")
	tenant := "t4-ccda"
	base := ts.URL + "/fhir/r4/" + tenant

	oncCCD := readTrack4Vector(t, "pkg/ccda/testdata/onc_b1_ccd_r21.xml")

	// 1. Raw application/cda+xml $import-ccda (idempotent twice)
	var importedPatientID string
	for attempt := 1; attempt <= 2; attempt++ {
		stImp, _, bImp := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/$import-ccda", "application/cda+xml", "application/fhir+json", oncCCD, nil)
		require.Equal(t, http.StatusOK, stImp, "attempt %d body=%s", attempt, string(bImp))
		var respBundle map[string]any
		require.NoError(t, json.Unmarshal(bImp, &respBundle))
		assert.Equal(t, "transaction-response", respBundle["type"])
		if importedPatientID == "" {
			for _, rawE := range respBundle["entry"].([]any) {
				em := rawE.(map[string]any)
				if fu, _ := em["fullUrl"].(string); strings.HasPrefix(fu, "Patient/") {
					importedPatientID = strings.TrimPrefix(fu, "Patient/")
					break
				}
			}
		}
	}
	require.NotEmpty(t, importedPatientID, "expected Patient ID in $import-ccda transaction-response")

	// 2. Non-persisting document mode (?persist=false) returns Composition at entry[0]
	stDoc, _, bDoc := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/$import-ccda?persist=false", "application/cda+xml", "application/fhir+json", oncCCD, nil)
	require.Equal(t, http.StatusOK, stDoc, string(bDoc))
	var docBundle map[string]any
	require.NoError(t, json.Unmarshal(bDoc, &docBundle))
	assert.Equal(t, "document", docBundle["type"])
	docEntries := docBundle["entry"].([]any)
	require.NotEmpty(t, docEntries)
	assert.Equal(t, "Composition", docEntries[0].(map[string]any)["resource"].(map[string]any)["resourceType"])

	// 3. Parameters (JSON, FHIR XML, Binary Proto3), Binary, and DocumentReference wrappers on $import-ccda
	ccdaParamsJSON, err := json.Marshal(map[string]any{
		"resourceType": "Parameters",
		"parameter": []map[string]any{
			{"name": "ccda", "valueString": string(oncCCD)},
			{"name": "persist", "valueBoolean": false},
		},
	})
	require.NoError(t, err)
	stPJ, _, _ := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/$import-ccda", "application/fhir+json", "application/fhir+json", ccdaParamsJSON, nil)
	require.Equal(t, http.StatusOK, stPJ)

	stCXML, _, paramsXML := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/Parameters/$convert", "application/fhir+json", "application/fhir+xml", ccdaParamsJSON, nil)
	require.Equal(t, http.StatusOK, stCXML)
	stPXML, hdrPXML, _ := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/$import-ccda", "application/fhir+xml", "application/fhir+xml", paramsXML, nil)
	require.Equal(t, http.StatusOK, stPXML)
	assert.Contains(t, strings.ToLower(hdrPXML.Get("Content-Type")), "application/fhir+xml")

	stCPB, _, paramsPB := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/Parameters/$convert", "application/fhir+json", "application/x-protobuf", ccdaParamsJSON, nil)
	require.Equal(t, http.StatusOK, stCPB)
	stPPB, hdrPPB, _ := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/$import-ccda", "application/x-protobuf", "application/x-protobuf", paramsPB, nil)
	require.Equal(t, http.StatusOK, stPPB)
	assert.Contains(t, strings.ToLower(hdrPPB.Get("Content-Type")), "application/x-protobuf")

	encodedCCD := base64.StdEncoding.EncodeToString(oncCCD)
	binBody, _ := json.Marshal(map[string]any{
		"resourceType": "Binary",
		"contentType":  "application/cda+xml",
		"data":         encodedCCD,
	})
	stBin, _, _ := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/$import-ccda?mode=document", "application/fhir+json", "application/fhir+json", binBody, nil)
	require.Equal(t, http.StatusOK, stBin)

	docRefBody, _ := json.Marshal(map[string]any{
		"resourceType": "DocumentReference",
		"status":       "current",
		"content": []map[string]any{
			{"attachment": map[string]any{"contentType": "application/cda+xml", "data": encodedCCD}},
		},
	})
	stDR, _, _ := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/$import-ccda?mode=document", "application/fhir+json", "application/fhir+json", docRefBody, nil)
	require.Equal(t, http.StatusOK, stDR)

	// 4. XXE / DOCTYPE / ENTITY rejection across both raw XML and base64-wrapped Binary
	xxeRaw := []byte(`<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE ClinicalDocument [<!ENTITY xxe SYSTEM "file:///etc/passwd">]><ClinicalDocument xmlns="urn:hl7-org:v3"><title>&xxe;</title></ClinicalDocument>`)
	stXXE1, _, bXXE1 := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/$import-ccda", "application/cda+xml", "application/fhir+json", xxeRaw, nil)
	require.Equal(t, http.StatusBadRequest, stXXE1)
	assert.Contains(t, string(bXXE1), `"security"`)

	xxeWrapped, _ := json.Marshal(map[string]any{
		"resourceType": "Binary",
		"contentType":  "application/cda+xml",
		"data":         base64.StdEncoding.EncodeToString(xxeRaw),
	})
	stXXE2, _, bXXE2 := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/$import-ccda", "application/fhir+json", "application/fhir+json", xxeWrapped, nil)
	require.Equal(t, http.StatusBadRequest, stXXE2)
	assert.Contains(t, string(bXXE2), `"security"`)

	oversized := bytes.Repeat([]byte(" "), ccda.MaxDocumentBytes+10)
	stBig, _, _ := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/$import-ccda", "application/cda+xml", "application/fhir+json", oversized, nil)
	require.Equal(t, http.StatusRequestEntityTooLarge, stBig)

	// 5. Export via GET & POST Patient/{id}/$ccda and Patient/$ccda -> valid C-CDA R2.1 XML across all 8 sections
	stExp, hdrExp, exportedXML := doTrack4HTTP(t, ts.Client(), http.MethodGet, base+"/Patient/"+importedPatientID+"/$ccda", "", "application/cda+xml", nil, nil)
	require.Equal(t, http.StatusOK, stExp, string(exportedXML))
	assert.Contains(t, strings.ToLower(hdrExp.Get("Content-Type")), "application/cda+xml")
	assert.Equal(t, "hl7.ccda.r2.1.xml", hdrExp.Get("X-Flint-Wire-Format"))

	parsedBack, err := ccda.Parse(exportedXML)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(parsedBack.Component.StructuredBody.Components), 8)

	// Type-level GET & POST Patient/$ccda
	stTypeGet, _, _ := doTrack4HTTP(t, ts.Client(), http.MethodGet, base+"/Patient/$ccda?patient="+url.QueryEscape("Patient/"+importedPatientID), "", "application/cda+xml", nil, nil)
	require.Equal(t, http.StatusOK, stTypeGet)

	postTypeParams, _ := json.Marshal(map[string]any{
		"resourceType": "Parameters",
		"parameter": []map[string]any{
			{"name": "patient", "valueReference": map[string]any{"reference": "Patient/" + importedPatientID}},
			{"name": "title", "valueString": "Custom Referral Summary"},
		},
	})
	stTypePost, _, bTypePost := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/Patient/$ccda", "application/fhir+json", "application/cda+xml", postTypeParams, nil)
	require.Equal(t, http.StatusOK, stTypePost)
	assert.Contains(t, string(bTypePost), "Custom Referral Summary")

	// FHIR-wrapped export (JSON, FHIR XML, Binary Proto3 Parameters + DocumentReference)
	for _, accept := range []string{"application/fhir+json", "application/fhir+xml", "application/x-protobuf"} {
		stW, hdrW, bW := doTrack4HTTP(t, ts.Client(), http.MethodGet, base+"/Patient/"+importedPatientID+"/$ccda", "", accept, nil, nil)
		require.Equal(t, http.StatusOK, stW, "accept=%s body=%s", accept, string(bW))
		assert.Contains(t, strings.ToLower(hdrW.Get("Content-Type")), strings.ToLower(accept))
		assert.NotEmpty(t, bW)
	}

	// Missing patient ID -> 400; nonexistent patient -> 404
	stMiss, _, _ := doTrack4HTTP(t, ts.Client(), http.MethodGet, base+"/Patient/$ccda", "", "application/fhir+json", nil, nil)
	assert.Equal(t, http.StatusBadRequest, stMiss)

	stNotFound, _, _ := doTrack4HTTP(t, ts.Client(), http.MethodGet, base+"/Patient/no-such-ccda-patient/$ccda", "", "application/fhir+json", nil, nil)
	assert.Equal(t, http.StatusNotFound, stNotFound)

	// Verify C-CDA Prometheus counters incremented
	stMet, _, metBody := doTrack4HTTP(t, ts.Client(), http.MethodGet, ts.URL+"/metrics", "", "text/plain", nil, nil)
	require.Equal(t, http.StatusOK, stMet)
	metStr := string(metBody)
	assert.Contains(t, metStr, "flint_ccda_documents_imported_total ")
	assert.NotContains(t, metStr, "flint_ccda_documents_imported_total 0\n")
	assert.Contains(t, metStr, "flint_ccda_documents_exported_total ")
	assert.NotContains(t, metStr, "flint_ccda_documents_exported_total 0\n")
	assert.Contains(t, metStr, "flint_ccda_operations_errors_total ")
	assert.NotContains(t, metStr, "flint_ccda_operations_errors_total 0\n")
}

func TestHL7v2AndCCDA_SMARTv2ScopesAndCapabilityStatement(t *testing.T) {
	srv, ts, _ := newTrack4TestServer(t, "smart-t4")
	tenant := "t4-smart"
	base := ts.URL + "/fhir/r4/" + tenant

	// 1. Seed Patient + read CapabilityStatement + ConformanceMatrix before enabling token gate
	stMeta, _, metaBody := doTrack4HTTP(t, ts.Client(), http.MethodGet, base+"/metadata", "", "application/fhir+json", nil, nil)
	require.Equal(t, http.StatusOK, stMeta)
	metaStr := string(metaBody)
	assert.Contains(t, metaStr, `"ingest-hl7v2"`)
	assert.Contains(t, metaStr, `"import-ccda"`)
	assert.Contains(t, metaStr, `"ccda"`)

	var matrixHasBridge, matrixHas144 bool
	for _, entry := range server.ConformanceMatrix() {
		if strings.Contains(entry.Category, "Legacy Clinical Bridge (HL7 v2.x MLLP/HTTP & C-CDA R2.1)") {
			matrixHasBridge = true
		}
		if strings.Contains(entry.Capability, "144-Check") {
			matrixHas144 = true
		}
	}
	assert.True(t, matrixHasBridge, "expected ConformanceMatrix to advertise HL7 v2.x & C-CDA R2.1 bridge")
	assert.True(t, matrixHas144, "expected ConformanceMatrix to advertise 144 checks")

	// Seed target patient and another patient via SMART-free PUT before enforcing auth
	stSeed1, _, _ := doTrack4HTTP(t, ts.Client(), http.MethodPut, base+"/Patient/pat-one", "application/fhir+json", "application/fhir+json",
		[]byte(`{"resourceType":"Patient","id":"pat-one","active":true,"name":[{"family":"One"}]}`), nil)
	require.Contains(t, []int{http.StatusOK, http.StatusCreated}, stSeed1)
	stSeed2, _, _ := doTrack4HTTP(t, ts.Client(), http.MethodPut, base+"/Patient/pat-two", "application/fhir+json", "application/fhir+json",
		[]byte(`{"resourceType":"Patient","id":"pat-two","active":true,"name":[{"family":"Two"}]}`), nil)
	require.Contains(t, []int{http.StatusOK, http.StatusCreated}, stSeed2)

	srv.EnableDevTokenEnforcement()

	noRedir := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	issueToken := func(patientID, scopes string) string {
		t.Helper()
		authURL := ts.URL + "/oauth2/authorize?response_type=code&client_id=t4-cli&redirect_uri=" +
			url.QueryEscape("https://t4.example/cb") +
			"&scope=" + url.QueryEscape(scopes) + "&state=t4s&patient=" + url.QueryEscape(patientID)
		respAuth, err := noRedir.Get(authURL)
		require.NoError(t, err)
		loc := respAuth.Header.Get("Location")
		respAuth.Body.Close()
		require.Equal(t, http.StatusFound, respAuth.StatusCode)
		u, err := url.Parse(loc)
		require.NoError(t, err)
		code := u.Query().Get("code")
		require.NotEmpty(t, code)

		form := url.Values{
			"grant_type":   {"authorization_code"},
			"code":         {code},
			"redirect_uri": {"https://t4.example/cb"},
		}
		respTok, err := ts.Client().Post(ts.URL+"/oauth2/token", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
		require.NoError(t, err)
		var tokMap map[string]any
		require.NoError(t, json.NewDecoder(respTok.Body).Decode(&tokMap))
		respTok.Body.Close()
		require.Equal(t, http.StatusOK, respTok.StatusCode)
		tok, _ := tokMap["access_token"].(string)
		require.NotEmpty(t, tok)
		return tok
	}

	readOnlyTok := issueToken("pat-one", "user/*.rs")
	patOneTok := issueToken("pat-one", "patient/Patient.rs")

	// System-level client_credentials token (no patient claim restriction) for multi-resource $ingest-hl7v2 / $import-ccda writes
	ccForm := url.Values{
		"grant_type": {"client_credentials"},
		"scope":      {"system/*.cruds"},
	}
	respCC, err := ts.Client().Post(ts.URL+"/oauth2/token", "application/x-www-form-urlencoded", strings.NewReader(ccForm.Encode()))
	require.NoError(t, err)
	var ccMap map[string]any
	require.NoError(t, json.NewDecoder(respCC.Body).Decode(&ccMap))
	respCC.Body.Close()
	require.Equal(t, http.StatusOK, respCC.StatusCode)
	writeTok, _ := ccMap["access_token"].(string)
	require.NotEmpty(t, writeTok)

	adtMsg := []byte("MSH|^~\\&|EHR|FAC|FLINT|HOSP|20261119120000+0000||ADT^A01^ADT_A01|SMART-HL7-1|P|2.5.1\rPID|1||SMRT-1^^^FAC^MR||Smart^User||19850101|F\r")
	oncCCD := readTrack4Vector(t, "pkg/ccda/testdata/onc_b1_ccd_r21.xml")

	// Read-only token is rejected 403 by internal transaction Bundle SMART scope enforcement
	stHL7RO, _, _ := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/$ingest-hl7v2", "x-application/hl7-v2+er7", "application/fhir+json", adtMsg,
		map[string]string{"Authorization": "Bearer " + readOnlyTok})
	assert.Equal(t, http.StatusForbidden, stHL7RO)

	stCCDARO, _, _ := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/$import-ccda", "application/cda+xml", "application/fhir+json", oncCCD,
		map[string]string{"Authorization": "Bearer " + readOnlyTok})
	assert.Equal(t, http.StatusForbidden, stCCDARO)

	// Patient-scoped token cannot export another patient's $ccda, but can export its own
	stCrossPat, _, _ := doTrack4HTTP(t, ts.Client(), http.MethodGet, base+"/Patient/pat-two/$ccda", "", "application/cda+xml", nil,
		map[string]string{"Authorization": "Bearer " + patOneTok})
	assert.Equal(t, http.StatusForbidden, stCrossPat)

	stOwnPat, _, _ := doTrack4HTTP(t, ts.Client(), http.MethodGet, base+"/Patient/pat-one/$ccda", "", "application/cda+xml", nil,
		map[string]string{"Authorization": "Bearer " + patOneTok})
	assert.Equal(t, http.StatusOK, stOwnPat)

	// Authorized write token succeeds on both $ingest-hl7v2 and $import-ccda
	stHL7RW, _, _ := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/$ingest-hl7v2", "x-application/hl7-v2+er7", "application/fhir+json", adtMsg,
		map[string]string{"Authorization": "Bearer " + writeTok})
	assert.Equal(t, http.StatusOK, stHL7RW)

	stCCDARW, _, _ := doTrack4HTTP(t, ts.Client(), http.MethodPost, base+"/$import-ccda", "application/cda+xml", "application/fhir+json", oncCCD,
		map[string]string{"Authorization": "Bearer " + writeTok})
	assert.Equal(t, http.StatusOK, stCCDARW)
}
