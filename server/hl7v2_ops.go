package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/flint-fhir/flint/pkg/audit"
	"github.com/flint-fhir/flint/pkg/hl7v2"
	"github.com/flint-fhir/flint/pkg/telemetry"
	"github.com/flint-fhir/flint/store"
)

const hl7v2ER7ContentType = "x-application/hl7-v2+er7; charset=utf-8"

func wantsHL7v2Response(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.URL != nil {
		fmtParam := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("_format")))
		if strings.Contains(fmtParam, "hl7-v2") || fmtParam == "hl7v2" || fmtParam == "er7" {
			return true
		}
	}
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "hl7-v2")
}

// executeInternalTransactionBundle runs a FHIR transaction Bundle through the server's
// own authMiddleware + fhirRouter pipeline so SMART v2 scopes, compartment checks, validation,
// index extraction, subscriptions, and CDC operate identically to REST requests.
func (s *Server) executeInternalTransactionBundle(
	ctx context.Context,
	tenant string,
	authHeader string,
	purposeOfUse string,
	txBundle map[string]any,
	bypassAuth bool,
) (statusCode int, respBody []byte, respMap map[string]any, err error) {
	if strings.TrimSpace(tenant) == "" {
		tenant = "default"
	}
	payload, err := json.Marshal(txBundle)
	if err != nil {
		return http.StatusInternalServerError, nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://flint.internal/fhir/r4/"+tenant, bytes.NewReader(payload))
	if err != nil {
		return http.StatusInternalServerError, nil, nil, err
	}
	req.Header.Set("Content-Type", "application/fhir+json")
	req.Header.Set("Accept", "application/fhir+json")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	if purposeOfUse != "" {
		req.Header.Set("X-Purpose-Of-Use", purposeOfUse)
	}
	rec := httptest.NewRecorder()
	if bypassAuth {
		s.fhirRouter().ServeHTTP(rec, req)
	} else {
		s.authMiddleware(s.fhirRouter()).ServeHTTP(rec, req)
	}
	respBytes := bytes.TrimSpace(rec.Body.Bytes())
	var outMap map[string]any
	if len(respBytes) > 0 {
		_ = json.Unmarshal(respBytes, &outMap)
	}
	return rec.Code, respBytes, outMap, nil
}

// handleIngestHL7v2 implements POST /fhir/r4/{tenant}/$ingest-hl7v2.
func (s *Server) handleIngestHL7v2(w http.ResponseWriter, r *http.Request) {
	tenant := strings.TrimSpace(r.PathValue("tenant"))
	if tenant == "" {
		tenant = "default"
	}

	rawBody, err := io.ReadAll(io.LimitReader(r.Body, hl7v2.MaxMessageBytes+1))
	if err != nil {
		if s.metrics != nil {
			s.metrics.hl7v2ErrorsTotal.Add(1)
		}
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read HL7 v2 request body")
		return
	}
	if len(rawBody) > hl7v2.MaxMessageBytes {
		if s.metrics != nil {
			s.metrics.hl7v2ErrorsTotal.Add(1)
		}
		writeOperationOutcome(w, http.StatusRequestEntityTooLarge, "too-long", "HL7 v2 payload exceeds maximum size limit")
		return
	}

	rawHL7, err := s.extractHL7v2Payload(r, rawBody)
	if err != nil {
		if s.metrics != nil {
			s.metrics.hl7v2ErrorsTotal.Add(1)
		}
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	msg, parseErr := hl7v2.Parse(rawHL7)
	if parseErr != nil {
		if s.metrics != nil {
			s.metrics.hl7v2ErrorsTotal.Add(1)
		}
		nack := hl7v2.BuildACK(nil, rawHL7, hl7v2.ACKReject, parseErr.Error(), "100", time.Now().UTC())
		s.writeHL7v2Error(w, r, http.StatusBadRequest, nack, parseErr.Error())
		return
	}

	mapRes, mapErr := hl7v2.MapToFHIRBundle(msg, hl7v2.MapOptions{Tenant: tenant})
	if mapErr != nil {
		if s.metrics != nil {
			s.metrics.hl7v2ErrorsTotal.Add(1)
		}
		ackCode := hl7v2.ACKError
		errCode := "101"
		if errors.Is(mapErr, hl7v2.ErrUnsupportedMessageType) {
			ackCode = hl7v2.ACKReject
			errCode = "200"
		}
		nack := hl7v2.BuildACK(msg, rawHL7, ackCode, mapErr.Error(), errCode, time.Now().UTC())
		s.writeHL7v2Error(w, r, http.StatusBadRequest, nack, mapErr.Error())
		return
	}

	if mapRes.PatientID != "" {
		setAuditPatientID(r.Context(), mapRes.PatientID)
	}

	txStatus, txBody, txRespMap, txErr := s.executeInternalTransactionBundle(
		r.Context(),
		tenant,
		r.Header.Get("Authorization"),
		r.Header.Get("X-Purpose-Of-Use"),
		mapRes.Bundle,
		false,
	)
	if txErr != nil {
		if s.metrics != nil {
			s.metrics.hl7v2ErrorsTotal.Add(1)
		}
		nack := hl7v2.BuildACK(msg, rawHL7, hl7v2.ACKError, txErr.Error(), "207", time.Now().UTC())
		s.writeHL7v2Error(w, r, http.StatusInternalServerError, nack, txErr.Error())
		return
	}
	if txStatus == http.StatusUnauthorized || txStatus == http.StatusForbidden {
		w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
		w.WriteHeader(txStatus)
		_, _ = w.Write(txBody)
		return
	}
	if txStatus >= 400 {
		if s.metrics != nil {
			s.metrics.hl7v2ErrorsTotal.Add(1)
		}
		nack := hl7v2.BuildACK(msg, rawHL7, hl7v2.ACKError, fmt.Sprintf("FHIR transaction failed (status %d)", txStatus), "207", time.Now().UTC())
		s.writeHL7v2Error(w, r, txStatus, nack, string(txBody))
		return
	}

	if s.metrics != nil {
		s.metrics.hl7v2IngestedTotal.Add(1)
	}
	ackStr := hl7v2.BuildACK(msg, rawHL7, hl7v2.ACKAccept, "Message committed to Flint FHIR R4", "", time.Now().UTC())
	if wantsHL7v2Response(r) {
		w.Header().Set("Content-Type", hl7v2ER7ContentType)
		w.Header().Set("X-Flint-Wire-Format", "hl7.v2.er7")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(ackStr))
		return
	}

	params := map[string]any{
		"resourceType": "Parameters",
		"parameter": []map[string]any{
			{"name": "ack", "valueString": ackStr},
			{"name": "ackCode", "valueCode": hl7v2.ACKAccept},
			{"name": "messageType", "valueString": mapRes.MessageType},
			{"name": "messageControlId", "valueString": mapRes.ControlID},
			{"name": "patient", "valueReference": map[string]any{"reference": "Patient/" + mapRes.PatientID}},
			{"name": "bundle", "resource": txRespMap},
		},
	}
	s.writeOperationResponse(w, r, http.StatusOK, "Parameters", params)
}

func (s *Server) extractHL7v2Payload(r *http.Request, body []byte) (string, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return "", hl7v2.ErrEmptyMessage
	}
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if strings.Contains(ct, "hl7-v2") || strings.Contains(ct, "text/plain") || bytes.HasPrefix(trimmed, []byte("MSH")) {
		return string(body), nil
	}

	bodyMap, err := s.decodeOperationBodyMap(r, body, "Parameters")
	if err != nil {
		// Fall back cleanly if client sent malformed HL7 without MSH header and content-type wasn't JSON/XML
		if !isProtoContentType(r) && !strings.Contains(ct, "json") && !strings.Contains(ct, "xml") {
			return string(body), nil
		}
		return "", fmt.Errorf("decode $ingest-hl7v2 request: %w", err)
	}
	if rt, _ := bodyMap["resourceType"].(string); rt == "Parameters" {
		if rawParams, ok := bodyMap["parameter"].([]any); ok {
			for _, item := range rawParams {
				pm, ok := item.(map[string]any)
				if !ok {
					continue
				}
				name, _ := pm["name"].(string)
				if name == "message" || name == "hl7v2" || name == "data" {
					if vs := firstStringValue(pm, "valueString"); vs != "" {
						return vs, nil
					}
					if b64 := firstStringValue(pm, "valueBase64Binary"); b64 != "" {
						if dec, dErr := base64.StdEncoding.DecodeString(b64); dErr == nil {
							return string(dec), nil
						}
					}
				}
			}
		}
	}
	if msgStr, _ := bodyMap["message"].(string); strings.TrimSpace(msgStr) != "" {
		return msgStr, nil
	}
	return "", errors.New("missing 'message' HL7 v2 ER7 payload")
}

func (s *Server) writeHL7v2Error(w http.ResponseWriter, r *http.Request, status int, nack string, diag string) {
	if wantsHL7v2Response(r) {
		w.Header().Set("Content-Type", hl7v2ER7ContentType)
		w.Header().Set("X-Flint-Wire-Format", "hl7.v2.er7")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(nack))
		return
	}
	writeOperationOutcome(w, status, "invalid", fmt.Sprintf("%s | ACK: %s", diag, strings.ReplaceAll(nack, "\r", "\\r")))
}

// IngestHL7v2MLLP parses, maps, commits to FHIR R4 storage, emits an OpenTelemetry span
// and ASTM E2147 AuditEvent, and returns the ER7 ACK/NACK message for a raw MLLP payload.
func (s *Server) IngestHL7v2MLLP(ctx context.Context, tenant string, rawMsg []byte) ([]byte, error) {
	if strings.TrimSpace(tenant) == "" {
		tenant = "default"
	}
	if ctx == nil {
		ctx = context.Background()
	}

	spanCtx := ctx
	var sp *telemetry.Span
	if s.tracer != nil && s.tracer.Enabled() {
		spanCtx, sp = s.tracer.Start(
			ctx,
			"MLLP INGEST",
			telemetry.SpanKindServer,
			telemetry.String("rpc.system", "hl7v2_mllp"),
			telemetry.String("fhir.tenant", tenant),
		)
	}
	finishSpan := func(statusCode int, patID, msgType, msgErrStr string) {
		if sp != nil {
			if patID != "" {
				sp.SetAttributes(telemetry.String("fhir.patient_id", patID))
			}
			if msgType != "" {
				sp.SetAttributes(telemetry.String("hl7v2.message_type", msgType))
			}
			if statusCode >= 400 {
				sp.SetStatus(telemetry.StatusError, msgErrStr)
			} else {
				sp.SetStatus(telemetry.StatusOK, "")
			}
			sp.End()
		}
		if s.auditRecorder != nil {
			outcome := audit.OutcomeSuccess
			if statusCode >= 400 {
				outcome = audit.OutcomeSeriousFailure
			}
			desc := "MLLP HL7 v2 message ingested"
			if msgErrStr != "" {
				desc = msgErrStr
			}
			_ = s.auditRecorder.Record(spanCtx, store.AuditRecord{
				TenantID:     tenant,
				AuditID:      uuid.NewString(),
				Recorded:     time.Now().UTC(),
				Action:       audit.ActionCreate,
				SubtypeCode:  "ingest-hl7v2-mllp",
				Outcome:      outcome,
				OutcomeDesc:  desc,
				HTTPMethod:   "MLLP",
				HTTPStatus:   statusCode,
				RequestURI:   "mllp://flint/fhir/r4/" + tenant + "/$ingest-hl7v2",
				AgentSubject: "mllp-listener",
				AgentPatient: patID,
				ClientIP:     "mllp",
				EntityType:   "Patient",
				EntityID:     patID,
			})
		}
	}

	rawStr := string(rawMsg)
	msg, err := hl7v2.ParseBytes(rawMsg)
	if err != nil {
		if s.metrics != nil {
			s.metrics.hl7v2ErrorsTotal.Add(1)
		}
		finishSpan(http.StatusBadRequest, "", "", err.Error())
		return []byte(hl7v2.BuildACK(nil, rawStr, hl7v2.ACKReject, err.Error(), "100", time.Now().UTC())), err
	}

	mapRes, err := hl7v2.MapToFHIRBundle(msg, hl7v2.MapOptions{Tenant: tenant})
	if err != nil {
		if s.metrics != nil {
			s.metrics.hl7v2ErrorsTotal.Add(1)
		}
		ackCode := hl7v2.ACKError
		errCode := "101"
		if errors.Is(err, hl7v2.ErrUnsupportedMessageType) {
			ackCode = hl7v2.ACKReject
			errCode = "200"
		}
		code, trigger, _ := msg.MessageType()
		mt := code
		if trigger != "" {
			mt = code + "^" + trigger
		}
		finishSpan(http.StatusBadRequest, "", mt, err.Error())
		return []byte(hl7v2.BuildACK(msg, rawStr, ackCode, err.Error(), errCode, time.Now().UTC())), err
	}

	txStatus, _, _, txErr := s.executeInternalTransactionBundle(spanCtx, tenant, "", "", mapRes.Bundle, true)
	if txErr != nil || txStatus >= 400 {
		if s.metrics != nil {
			s.metrics.hl7v2ErrorsTotal.Add(1)
		}
		errMsg := fmt.Sprintf("FHIR store transaction failed (%d)", txStatus)
		if txErr != nil {
			errMsg = txErr.Error()
		}
		finishSpan(max(txStatus, 500), mapRes.PatientID, mapRes.MessageType, errMsg)
		return []byte(hl7v2.BuildACK(msg, rawStr, hl7v2.ACKError, errMsg, "207", time.Now().UTC())), errors.New(errMsg)
	}

	if s.metrics != nil {
		s.metrics.hl7v2IngestedTotal.Add(1)
		s.metrics.hl7v2MLLPIngestedTotal.Add(1)
	}
	finishSpan(http.StatusOK, mapRes.PatientID, mapRes.MessageType, "")
	ack := hl7v2.BuildACK(msg, rawStr, hl7v2.ACKAccept, "Message committed to Flint FHIR R4", "", time.Now().UTC())
	return []byte(ack), nil
}

// NewMLLPServer constructs a TCP MLLP v1 server bound to addr that routes every
// incoming HL7 v2 message into tenant via IngestHL7v2MLLP.
func (s *Server) NewMLLPServer(addr, tenant string) *hl7v2.MLLPServer {
	if strings.TrimSpace(tenant) == "" {
		tenant = "default"
	}
	return hl7v2.NewMLLPServer(hl7v2.MLLPConfig{
		Addr:   addr,
		Logger: s.logger,
		Handler: func(ctx context.Context, raw []byte) ([]byte, error) {
			return s.IngestHL7v2MLLP(ctx, tenant, raw)
		},
	})
}
