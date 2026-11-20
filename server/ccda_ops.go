package server

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/pkg/ccda"
	"github.com/flint-fhir/flint/store"
)

const ccdaXMLContentType = "application/cda+xml; charset=utf-8"

// handleImportCCDA implements POST /fhir/r4/{tenant}/$import-ccda.
// It accepts either raw HL7 C-CDA R2.1 XML (application/cda+xml, application/xml, text/xml)
// or FHIR R4 Parameters / Binary / DocumentReference (JSON, FHIR XML, or Binary Proto3).
func (s *Server) handleImportCCDA(w http.ResponseWriter, r *http.Request) {
	tenant := strings.TrimSpace(r.PathValue("tenant"))
	if tenant == "" {
		tenant = "default"
	}

	rawBody, err := io.ReadAll(io.LimitReader(r.Body, ccda.MaxDocumentBytes+1))
	if err != nil {
		if s.metrics != nil {
			s.metrics.ccdaErrorsTotal.Add(1)
		}
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read C-CDA request body")
		return
	}
	if len(rawBody) > ccda.MaxDocumentBytes {
		if s.metrics != nil {
			s.metrics.ccdaErrorsTotal.Add(1)
		}
		writeOperationOutcome(w, http.StatusRequestEntityTooLarge, "too-long", "C-CDA payload exceeds maximum size limit")
		return
	}

	rawXML, persist, err := s.extractCCDAImportPayload(r, rawBody)
	if err != nil {
		if s.metrics != nil {
			s.metrics.ccdaErrorsTotal.Add(1)
		}
		code := "invalid"
		if errors.Is(err, ccda.ErrXXEForbidden) {
			code = "security"
		}
		writeOperationOutcome(w, http.StatusBadRequest, code, err.Error())
		return
	}

	mode := "transaction"
	if !persist || strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("_mode")), "document") ||
		strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("mode")), "document") ||
		strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("persist")), "false") {
		mode = "document"
	}

	res, err := ccda.ConvertToFHIR(rawXML, ccda.ImportOptions{Mode: mode})
	if err != nil {
		if s.metrics != nil {
			s.metrics.ccdaErrorsTotal.Add(1)
		}
		code := "invalid"
		if errors.Is(err, ccda.ErrXXEForbidden) {
			code = "security"
		}
		writeOperationOutcome(w, http.StatusBadRequest, code, err.Error())
		return
	}

	if res.PatientID != "" {
		setAuditPatientID(r.Context(), res.PatientID)
	}

	if mode == "document" {
		if secCtx, hasSec := auth.FromContext(r.Context()); hasSec {
			if !auth.Allows(secCtx, "Patient", auth.ActionRead) && !auth.Allows(secCtx, "Composition", auth.ActionRead) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient SMART scope for $import-ccda document conversion")
				return
			}
		}
		if s.metrics != nil {
			s.metrics.ccdaImportedTotal.Add(1)
		}
		s.writeOperationResponse(w, r, http.StatusOK, "Bundle", res.Bundle)
		return
	}

	txStatus, txBody, txRespMap, txErr := s.executeInternalTransactionBundle(
		r.Context(),
		tenant,
		r.Header.Get("Authorization"),
		r.Header.Get("X-Purpose-Of-Use"),
		res.Bundle,
		false,
	)
	if txErr != nil {
		if s.metrics != nil {
			s.metrics.ccdaErrorsTotal.Add(1)
		}
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", txErr.Error())
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
			s.metrics.ccdaErrorsTotal.Add(1)
		}
		w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
		w.WriteHeader(txStatus)
		_, _ = w.Write(txBody)
		return
	}

	if s.metrics != nil {
		s.metrics.ccdaImportedTotal.Add(1)
	}
	s.writeOperationResponse(w, r, http.StatusOK, "Bundle", txRespMap)
}

func (s *Server) extractCCDAImportPayload(r *http.Request, body []byte) ([]byte, bool, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, true, ccda.ErrEmptyDocument
	}
	lowerStr := strings.ToLower(string(trimmed))
	if strings.Contains(lowerStr, "<!doctype") || strings.Contains(lowerStr, "<!entity") {
		return nil, true, ccda.ErrXXEForbidden
	}

	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if strings.Contains(ct, "cda+xml") {
		return body, true, nil
	}
	if !isProtoContentType(r) && bytes.HasPrefix(trimmed, []byte("<")) {
		if rootLocal := sniffFirstXMLElementLocalName(trimmed); rootLocal == "ClinicalDocument" {
			return body, true, nil
		}
	}

	bodyMap, err := s.decodeOperationBodyMap(r, body, "Parameters")
	if err != nil {
		// Fall back if client sent non-JSON malformed XML directly
		if bytes.HasPrefix(trimmed, []byte("<")) {
			return body, true, nil
		}
		return nil, true, fmt.Errorf("decode $import-ccda request: %w", err)
	}

	persist := true
	rt, _ := bodyMap["resourceType"].(string)
	switch rt {
	case "Binary":
		if b64, _ := bodyMap["data"].(string); strings.TrimSpace(b64) != "" {
			dec, dErr := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
			return dec, persist, dErr
		}
	case "DocumentReference":
		if content, ok := bodyMap["content"].([]any); ok && len(content) > 0 {
			if c0, ok := content[0].(map[string]any); ok {
				if att, ok := c0["attachment"].(map[string]any); ok {
					if b64, _ := att["data"].(string); strings.TrimSpace(b64) != "" {
						dec, dErr := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
						return dec, persist, dErr
					}
				}
			}
		}
	case "Parameters":
		var ccdaBytes []byte
		if rawParams, ok := bodyMap["parameter"].([]any); ok {
			for _, item := range rawParams {
				pm, ok := item.(map[string]any)
				if !ok {
					continue
				}
				name, _ := pm["name"].(string)
				switch name {
				case "ccda", "document":
					if vs := firstStringValue(pm, "valueString"); vs != "" {
						ccdaBytes = []byte(vs)
					} else if b64 := firstStringValue(pm, "valueBase64Binary"); b64 != "" {
						if dec, dErr := base64.StdEncoding.DecodeString(b64); dErr == nil {
							ccdaBytes = dec
						}
					} else if resMap, ok := pm["resource"].(map[string]any); ok {
						if b64, _ := resMap["data"].(string); b64 != "" {
							if dec, dErr := base64.StdEncoding.DecodeString(b64); dErr == nil {
								ccdaBytes = dec
							}
						}
					}
				case "persist":
					if bVal, ok := pm["valueBoolean"].(bool); ok {
						persist = bVal
					}
				case "mode":
					if strings.EqualFold(firstStringValue(pm, "valueString", "valueCode"), "document") {
						persist = false
					}
				}
			}
		}
		if len(ccdaBytes) > 0 {
			return ccdaBytes, persist, nil
		}
	}

	if sVal, _ := bodyMap["ccda"].(string); strings.TrimSpace(sVal) != "" {
		return []byte(sVal), persist, nil
	}
	if sVal, _ := bodyMap["document"].(string); strings.TrimSpace(sVal) != "" {
		return []byte(sVal), persist, nil
	}
	return nil, true, errors.New("missing 'ccda' or 'document' C-CDA R2.1 XML payload")
}

func sniffFirstXMLElementLocalName(raw []byte) string {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Entity = map[string]string{}
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		if se, ok := tok.(xml.StartElement); ok {
			return se.Name.Local
		}
	}
}

// handlePatientCCDA implements GET|POST /fhir/r4/{tenant}/Patient/{id}/$ccda
// and GET|POST /fhir/r4/{tenant}/Patient/$ccda, generating a C-CDA R2.1 Continuity
// of Care Document from the patient's FHIR R4 compartment.
func (s *Server) handlePatientCCDA(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeOperationOutcome(w, http.StatusServiceUnavailable, "exception", "store not configured")
		return
	}
	tenant := strings.TrimSpace(r.PathValue("tenant"))
	if tenant == "" {
		tenant = "default"
	}
	patientID := cleanID(strings.TrimSpace(r.PathValue("id")))
	if patientID == "" {
		q := r.URL.Query()
		for _, k := range []string{"patient", "id", "subject"} {
			if v := strings.TrimSpace(q.Get(k)); v != "" {
				patientID = cleanID(strings.TrimPrefix(v, "Patient/"))
				break
			}
		}
	}

	var docTitle string
	if r.Method == http.MethodPost && r.Body != nil {
		if rawBody, err := io.ReadAll(io.LimitReader(r.Body, 2<<20)); err == nil && len(bytes.TrimSpace(rawBody)) > 0 {
			if bodyMap, decErr := s.decodeOperationBodyMap(r, rawBody, "Parameters"); decErr == nil {
				if params, ok := bodyMap["parameter"].([]any); ok {
					for _, rawP := range params {
						pm, ok := rawP.(map[string]any)
						if !ok {
							continue
						}
						name, _ := pm["name"].(string)
						switch name {
						case "patient", "id", "subject":
							if patientID == "" {
								if v := extractParamString(pm); v != "" {
									patientID = cleanID(strings.TrimPrefix(v, "Patient/"))
								}
							}
						case "title":
							docTitle = firstStringValue(pm, "valueString")
						}
					}
				}
			}
		}
	}

	if patientID == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "Patient ID is required for $ccda")
		return
	}

	secCtx, hasSec := auth.FromContext(r.Context())
	if hasSec {
		if !auth.Allows(secCtx, "Patient", auth.ActionRead) && !auth.Allows(secCtx, "Patient", auth.ActionSearch) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient SMART scope for Patient/$ccda")
			return
		}
		if auth.IsPatientRestricted(secCtx) && cleanID(secCtx.GetPatientId()) != patientID {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				fmt.Sprintf("patient-scoped token (%s) cannot access $ccda for Patient/%s", secCtx.GetPatientId(), patientID))
			return
		}
	}

	setAuditPatientID(r.Context(), patientID)

	sr, err := s.store.PatientEverything(r.Context(), store.PatientEverythingParams{
		TenantID:  tenant,
		PatientID: patientID,
		Count:     500,
	})
	if errors.Is(err, sql.ErrNoRows) {
		writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("Patient/%s not found", patientID))
		return
	}
	if errors.Is(err, store.ErrResourceDeleted) {
		writeOperationOutcome(w, http.StatusGone, "deleted", fmt.Sprintf("Patient/%s has been deleted", patientID))
		return
	}
	if err != nil {
		if s.metrics != nil {
			s.metrics.ccdaErrorsTotal.Add(1)
		}
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to assemble Patient/$ccda")
		return
	}

	if hasSec && len(sr.Includes) > 0 {
		sr.Includes = s.filterAuthorizedIncludes(r, tenant, secCtx, sr.Includes)
	}

	var patientMap map[string]any
	if len(sr.Matches) > 0 {
		patientMap, _ = s.protoToFHIRMapWithMeta("Patient", sr.Matches[0].ResourceProto, sr.Matches[0].ResVersion, sr.Matches[0].LastUpdated)
	}
	if patientMap == nil {
		patientMap = map[string]any{"resourceType": "Patient", "id": patientID}
	}

	clinicalMaps := make([]map[string]any, 0, len(sr.Includes))
	for _, rec := range sr.Includes {
		if m, mErr := s.protoToFHIRMapWithMeta(rec.ResType, rec.ResourceProto, rec.ResVersion, rec.LastUpdated); mErr == nil && m != nil {
			clinicalMaps = append(clinicalMaps, m)
		}
	}

	ccdaXML, err := ccda.ConvertFHIRToCCDA(patientMap, clinicalMaps, ccda.ExportOptions{
		Title: docTitle,
	})
	if err != nil {
		if s.metrics != nil {
			s.metrics.ccdaErrorsTotal.Add(1)
		}
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", err.Error())
		return
	}
	if s.metrics != nil {
		s.metrics.ccdaExportedTotal.Add(1)
	}

	if wantsFHIRWrappedCCDA(r) {
		params := map[string]any{
			"resourceType": "Parameters",
			"parameter": []map[string]any{
				{"name": "ccda", "valueString": string(ccdaXML)},
				{
					"name": "documentReference",
					"resource": map[string]any{
						"resourceType": "DocumentReference",
						"status":       "current",
						"type": map[string]any{
							"coding": []map[string]any{
								{
									"system":  "http://loinc.org",
									"code":    "34133-9",
									"display": "Summarization of Episode Note",
								},
							},
						},
						"subject": map[string]any{"reference": "Patient/" + patientID},
						"date":    time.Now().UTC().Format(time.RFC3339),
						"content": []map[string]any{
							{
								"attachment": map[string]any{
									"contentType": "application/cda+xml",
									"data":        base64.StdEncoding.EncodeToString(ccdaXML),
								},
							},
						},
					},
				},
			},
		}
		s.writeOperationResponse(w, r, http.StatusOK, "Parameters", params)
		return
	}

	w.Header().Set("Content-Type", ccdaXMLContentType)
	w.Header().Set("X-Flint-Wire-Format", "hl7.ccda.r2.1.xml")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(ccdaXML)
	}
}

func wantsFHIRWrappedCCDA(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.URL != nil {
		fp := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("_format")))
		if fp != "" {
			if strings.Contains(fp, "cda") || fp == "xml" || fp == "text/xml" || fp == "application/xml" {
				return false
			}
			return strings.Contains(fp, "json") || strings.Contains(fp, "fhir+xml") || strings.Contains(fp, "protobuf")
		}
	}
	accept := strings.ToLower(strings.TrimSpace(strings.Join(r.Header.Values("Accept"), ",")))
	if accept == "" || accept == "*/*" {
		return false
	}
	if strings.Contains(accept, "application/cda+xml") {
		return false
	}
	return strings.Contains(accept, "application/fhir+json") ||
		strings.Contains(accept, "application/json") ||
		strings.Contains(accept, "application/fhir+xml") ||
		strings.Contains(accept, "application/x-protobuf") ||
		strings.Contains(accept, "application/fhir+protobuf")
}
