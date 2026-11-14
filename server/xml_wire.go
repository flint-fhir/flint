package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/flint-fhir/flint/pkg/fhirxml"
)

const fhirXMLContentType = "application/fhir+xml; charset=utf-8"

// isXMLContentType returns true when the request Content-Type indicates an HL7 FHIR XML payload
// (application/fhir+xml, application/xml, or text/xml).
func isXMLContentType(r *http.Request) bool {
	if r == nil {
		return false
	}
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if idx := strings.IndexByte(ct, ';'); idx >= 0 {
		ct = strings.TrimSpace(ct[:idx])
	}
	switch ct {
	case "application/fhir+xml", "application/xml", "text/xml":
		return true
	default:
		return false
	}
}

// wantsXMLResponse returns true and the negotiated Content-Type when the client requests HL7 FHIR R4
// XML wire format via ?_format=, Accept: quality-factor negotiation, or Content-Type fallback.
func wantsXMLResponse(r *http.Request) (bool, string) {
	if r == nil {
		return false, ""
	}
	isConvertOp := false
	if r.URL != nil {
		isConvertOp = strings.HasSuffix(r.URL.Path, "/$convert")
		formatParam := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("_format")))
		if formatParam == "" && isConvertOp {
			formatParam = strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
		}
		if idx := strings.IndexByte(formatParam, ';'); idx >= 0 {
			formatParam = strings.TrimSpace(formatParam[:idx])
		}
		formatParam = strings.ReplaceAll(formatParam, " ", "+")
		switch formatParam {
		case "xml", "text/xml", "application/xml", "application/fhir+xml", "fhir+xml":
			return true, fhirXMLContentType
		default:
			if formatParam != "" {
				return false, ""
			}
		}
	}

	rawAccept := strings.TrimSpace(strings.Join(r.Header.Values("Accept"), ","))
	if rawAccept == "" {
		if !isConvertOp && isXMLContentType(r) {
			return true, fhirXMLContentType
		}
		return false, ""
	}

	var fhirXMLQ, genericXMLQ, jsonQ, protoQ, htmlQ, wildcardQ float64
	for clause := range strings.SplitSeq(rawAccept, ",") {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		mediaType, q := parseAcceptMediaClause(clause)
		if q <= 0 {
			continue
		}
		switch mediaType {
		case "application/fhir+xml":
			if q > fhirXMLQ {
				fhirXMLQ = q
			}
		case "application/xml", "text/xml":
			if q > genericXMLQ {
				genericXMLQ = q
			}
		case "application/fhir+json", "application/json", "text/json":
			if q > jsonQ {
				jsonQ = q
			}
		case "application/fhir+protobuf", "application/x-protobuf", "application/protobuf", "application/vnd.google.protobuf":
			if q > protoQ {
				protoQ = q
			}
		case "text/html", "application/xhtml+xml":
			if q > htmlQ {
				htmlQ = q
			}
		case "*/*", "application/*":
			if q > wildcardQ {
				wildcardQ = q
			}
		}
	}

	if fhirXMLQ > 0 {
		if fhirXMLQ > jsonQ && fhirXMLQ >= protoQ {
			return true, fhirXMLContentType
		}
		if fhirXMLQ == jsonQ && fhirXMLQ >= protoQ && !isConvertOp && isXMLContentType(r) {
			return true, fhirXMLContentType
		}
		return false, ""
	}

	if genericXMLQ > 0 {
		// Require genericXMLQ >= htmlQ so standard web browser Accept headers
		// (text/html,application/xhtml+xml,application/xml;q=0.9) continue receiving JSON by default.
		if genericXMLQ > jsonQ && genericXMLQ >= protoQ && genericXMLQ >= htmlQ {
			return true, fhirXMLContentType
		}
		return false, ""
	}

	if wildcardQ > 0 && jsonQ == 0 && protoQ == 0 && htmlQ == 0 && !isConvertOp && isXMLContentType(r) {
		return true, fhirXMLContentType
	}
	return false, ""
}

func parseAcceptMediaClause(clause string) (string, float64) {
	parts := strings.Split(clause, ";")
	mediaType := strings.ToLower(strings.TrimSpace(parts[0]))
	q := 1.0
	for _, p := range parts[1:] {
		kv := strings.SplitN(strings.TrimSpace(p), "=", 2)
		if len(kv) == 2 && strings.EqualFold(strings.TrimSpace(kv[0]), "q") {
			parsed, err := strconv.ParseFloat(strings.TrimSpace(kv[1]), 64)
			if err != nil {
				return mediaType, 0
			}
			if parsed < 0 {
				parsed = 0
			}
			if parsed > 1 {
				parsed = 1
			}
			q = parsed
		}
	}
	return mediaType, q
}

func wantsXMLConvertOutput(targetFormat string, r *http.Request) bool {
	tf := strings.ToLower(strings.TrimSpace(targetFormat))
	if idx := strings.IndexByte(tf, ';'); idx >= 0 {
		tf = strings.TrimSpace(tf[:idx])
	}
	tf = strings.ReplaceAll(tf, " ", "+")
	switch tf {
	case "xml", "text/xml", "application/xml", "application/fhir+xml", "fhir+xml":
		return true
	}
	if tf != "" {
		return false
	}
	ok, _ := wantsXMLResponse(r)
	return ok
}

// decodeXMLBodyToFHIRJSON transcodes an incoming HL7 FHIR R4 XML request body into canonical
// HL7 FHIR R4 JSON bytes and optionally validates that the root element matches expectedResType.
func decodeXMLBodyToFHIRJSON(body []byte, expectedResType string) ([]byte, error) {
	jsonBytes, err := fhirxml.UnmarshalFHIRJSON(body)
	if err != nil {
		return nil, err
	}
	if expectedResType != "" {
		var hdr struct {
			ResourceType string `json:"resourceType"`
		}
		if uErr := json.Unmarshal(jsonBytes, &hdr); uErr == nil && hdr.ResourceType != "" && hdr.ResourceType != expectedResType {
			return nil, fmt.Errorf("resourceType mismatch: expected %q, got %q", expectedResType, hdr.ResourceType)
		}
	}
	return jsonBytes, nil
}

func decodeXMLBodyToFHIRMap(body []byte) (map[string]any, error) {
	return fhirxml.UnmarshalFHIRMap(body)
}

func writeFHIRXMLFromJSON(w http.ResponseWriter, r *http.Request, status int, jsonBytes []byte) bool {
	xmlBytes, err := fhirxml.MarshalFHIRJSON(jsonBytes)
	if err != nil {
		return false
	}
	w.Header().Set("Content-Type", fhirXMLContentType)
	w.Header().Set("X-Flint-Wire-Format", "hl7.fhir.r4.xml")
	w.WriteHeader(status)
	if r == nil || r.Method != http.MethodHead {
		_, _ = w.Write(xmlBytes)
	}
	return true
}

func writeFHIRXMLMap(w http.ResponseWriter, r *http.Request, status int, out map[string]any) bool {
	xmlBytes, err := fhirxml.MarshalFHIRMap(out)
	if err != nil {
		return false
	}
	w.Header().Set("Content-Type", fhirXMLContentType)
	w.Header().Set("X-Flint-Wire-Format", "hl7.fhir.r4.xml")
	w.WriteHeader(status)
	if r == nil || r.Method != http.MethodHead {
		_, _ = w.Write(xmlBytes)
	}
	return true
}

// xmlNegotiationMiddleware provides zero-overhead passthrough for non-XML requests and automatically
// transcodes any application/fhir+json response (including OperationOutcome errors and specialty $ operations)
// to application/fhir+xml when the client negotiated HL7 FHIR XML output.
func (s *Server) xmlNegotiationMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okXML, _ := wantsXMLResponse(r)
		if !okXML {
			next.ServeHTTP(w, r)
			return
		}
		xw := &xmlNegotiatingResponseWriter{
			ResponseWriter: w,
			req:            r,
		}
		next.ServeHTTP(xw, r)
		xw.finalize()
	})
}

type xmlNegotiatingResponseWriter struct {
	http.ResponseWriter
	req         *http.Request
	status      int
	wroteHeader bool
	passthrough bool
	buf         bytes.Buffer
}

func (xw *xmlNegotiatingResponseWriter) Unwrap() http.ResponseWriter {
	return xw.ResponseWriter
}

func (xw *xmlNegotiatingResponseWriter) shouldBufferAsFHIRJSON() bool {
	ct := strings.ToLower(strings.TrimSpace(xw.Header().Get("Content-Type")))
	return strings.HasPrefix(ct, "application/fhir+json")
}

func (xw *xmlNegotiatingResponseWriter) WriteHeader(code int) {
	if xw.wroteHeader {
		return
	}
	xw.wroteHeader = true
	xw.status = code
	if !xw.shouldBufferAsFHIRJSON() {
		xw.passthrough = true
		xw.ResponseWriter.WriteHeader(code)
	}
}

func (xw *xmlNegotiatingResponseWriter) Write(p []byte) (int, error) {
	if !xw.wroteHeader {
		xw.WriteHeader(http.StatusOK)
	}
	if xw.passthrough {
		return xw.ResponseWriter.Write(p)
	}
	return xw.buf.Write(p)
}

func (xw *xmlNegotiatingResponseWriter) finalize() {
	if xw.passthrough || !xw.wroteHeader {
		return
	}
	if xw.req != nil && xw.req.Method == http.MethodHead {
		xw.Header().Set("Content-Type", fhirXMLContentType)
		xw.Header().Set("X-Flint-Wire-Format", "hl7.fhir.r4.xml")
		xw.Header().Del("Content-Length")
		xw.ResponseWriter.WriteHeader(xw.status)
		return
	}
	raw := bytes.TrimSpace(xw.buf.Bytes())
	if len(raw) > 0 {
		if xmlBytes, err := fhirxml.MarshalFHIRJSON(raw); err == nil {
			xw.Header().Set("Content-Type", fhirXMLContentType)
			xw.Header().Set("X-Flint-Wire-Format", "hl7.fhir.r4.xml")
			xw.Header().Del("Content-Length")
			xw.ResponseWriter.WriteHeader(xw.status)
			_, _ = xw.ResponseWriter.Write(xmlBytes)
			return
		}
	}
	xw.ResponseWriter.WriteHeader(xw.status)
	if xw.buf.Len() > 0 {
		_, _ = xw.ResponseWriter.Write(xw.buf.Bytes())
	}
}
