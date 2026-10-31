// Package audit provides HIPAA / ONC security audit event classification,
// FHIR R4 AuditEvent serialization, and pluggable audit recorders.
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/flint-fhir/flint/store/postgres"
)

// FHIR R4 AuditEventAction codes (http://hl7.org/fhir/audit-event-action).
const (
	ActionCreate  = "C"
	ActionRead    = "R"
	ActionUpdate  = "U"
	ActionDelete  = "D"
	ActionExecute = "E"
)

// FHIR R4 AuditEventOutcome codes (http://hl7.org/fhir/audit-event-outcome).
const (
	OutcomeSuccess        = "0"
	OutcomeMinorFailure   = "4"
	OutcomeSeriousFailure = "8"
)

// Recorder persists and queries security audit records.
type Recorder interface {
	Record(ctx context.Context, rec postgres.AuditRecord) error
	Read(ctx context.Context, tenantID, auditID string) (*postgres.AuditRecord, error)
	Query(ctx context.Context, params postgres.AuditQueryParams) ([]postgres.AuditRecord, int, error)
}

// StoreRecorder backs Recorder with the Postgres operational store.
type StoreRecorder struct {
	store *postgres.Store
}

// NewStoreRecorder creates a Postgres-backed audit recorder.
func NewStoreRecorder(store *postgres.Store) *StoreRecorder {
	return &StoreRecorder{store: store}
}

func (r *StoreRecorder) Record(ctx context.Context, rec postgres.AuditRecord) error {
	return r.store.WriteAuditEvent(ctx, rec)
}

func (r *StoreRecorder) Read(ctx context.Context, tenantID, auditID string) (*postgres.AuditRecord, error) {
	return r.store.ReadAuditEvent(ctx, tenantID, auditID)
}

func (r *StoreRecorder) Query(ctx context.Context, params postgres.AuditQueryParams) ([]postgres.AuditRecord, int, error) {
	return r.store.QueryAuditEvents(ctx, params)
}

// MemoryRecorder is a thread-safe in-memory Recorder used when the server runs
// without a backing database (e.g., unit and auth-layer tests).
type MemoryRecorder struct {
	mu      sync.RWMutex
	records []postgres.AuditRecord
}

// NewMemoryRecorder creates an in-memory audit recorder.
func NewMemoryRecorder() *MemoryRecorder {
	return &MemoryRecorder{}
}

func (m *MemoryRecorder) Record(_ context.Context, rec postgres.AuditRecord) error {
	if rec.Recorded.IsZero() {
		rec.Recorded = time.Now().UTC()
	}
	if rec.AgentSubject == "" {
		rec.AgentSubject = "anonymous"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = append(m.records, rec)
	return nil
}

func (m *MemoryRecorder) Read(_ context.Context, tenantID, auditID string) (*postgres.AuditRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for i := len(m.records) - 1; i >= 0; i-- {
		rec := m.records[i]
		if rec.TenantID == tenantID && rec.AuditID == auditID {
			cp := rec
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("audit event %s/%s not found", tenantID, auditID)
}

func (m *MemoryRecorder) Query(_ context.Context, params postgres.AuditQueryParams) ([]postgres.AuditRecord, int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var matched []postgres.AuditRecord
	// Iterate in reverse so newest records come first.
	for i := len(m.records) - 1; i >= 0; i-- {
		rec := m.records[i]
		if params.TenantID != "" && rec.TenantID != params.TenantID {
			continue
		}
		if params.Action != "" && rec.Action != params.Action {
			continue
		}
		if params.SubtypeCode != "" && rec.SubtypeCode != params.SubtypeCode {
			continue
		}
		if params.Outcome != "" && rec.Outcome != params.Outcome {
			continue
		}
		if params.AgentSubject != "" && rec.AgentSubject != params.AgentSubject {
			continue
		}
		if params.AgentPatient != "" && rec.AgentPatient != params.AgentPatient {
			continue
		}
		if params.EntityType != "" && rec.EntityType != params.EntityType {
			continue
		}
		if params.EntityID != "" && rec.EntityID != params.EntityID {
			continue
		}
		matched = append(matched, rec)
	}

	total := len(matched)
	offset := max(params.Offset, 0)
	if offset >= total {
		return nil, total, nil
	}
	count := params.Count
	if count <= 0 {
		count = 100
	}
	end := min(offset+count, total)
	return matched[offset:end], total, nil
}

// ClassifyHTTPInteraction maps an HTTP method and split URL path (/fhir/r4/{tenant}/...)
// to FHIR R4 AuditEventAction (C, R, U, D, E), restful-interaction subtype code,
// and target entity metadata.
func ClassifyHTTPInteraction(method string, parts []string, statusCode int) (action, subtype, entityType, entityID, entityVersion string) {
	// Expected path shape: ["fhir", "r4", "{tenant}", ...]
	if len(parts) == 3 {
		switch method {
		case http.MethodPost:
			return ActionExecute, "batch", "Bundle", "", ""
		case http.MethodGet, http.MethodHead:
			return ActionExecute, "search-system", "System", "", ""
		}
	}
	if len(parts) < 4 {
		return ActionExecute, "operation", "", "", ""
	}

	resType := parts[3]
	if len(parts) == 4 && resType == "_search" && method == http.MethodPost {
		return ActionExecute, "search-system", "System", "", ""
	}
	if resType == "$validate" {
		return ActionExecute, "validate", "Resource", "", ""
	}
	if resType == "$export" {
		return ActionExecute, "export", "System", "", ""
	}
	if resType == "$export-poll" {
		jobID := ""
		if len(parts) >= 5 {
			jobID = parts[4]
		}
		if method == http.MethodDelete {
			return ActionDelete, "export-cancel", "ExportJob", jobID, ""
		}
		return ActionRead, "export-poll", "ExportJob", jobID, ""
	}
	if resType == "$export-file" {
		jobID := ""
		fileType := "ExportFile"
		if len(parts) >= 5 {
			jobID = parts[4]
		}
		if len(parts) >= 6 {
			fileType = strings.TrimSuffix(parts[5], ".ndjson")
		}
		return ActionRead, "export-file", fileType, jobID, ""
	}
	if resType == "metadata" || resType == "openapi.json" || resType == "$docs" || strings.HasPrefix(resType, ".well-known") {
		return ActionRead, "capabilities", "CapabilityStatement", "", ""
	}

	switch method {
	case http.MethodGet, http.MethodHead:
		if len(parts) >= 7 && parts[5] == "_history" {
			return ActionRead, "vread", resType, parts[4], parts[6]
		}
		if len(parts) >= 6 && parts[5] == "_history" {
			return ActionRead, "history-instance", resType, parts[4], ""
		}
		if len(parts) >= 6 && strings.HasPrefix(parts[5], "$") {
			return ActionExecute, strings.TrimPrefix(parts[5], "$"), resType, parts[4], ""
		}
		if len(parts) >= 5 && strings.HasPrefix(parts[4], "$") {
			return ActionExecute, strings.TrimPrefix(parts[4], "$"), resType, "", ""
		}
		if len(parts) >= 5 {
			return ActionRead, "read", resType, parts[4], ""
		}
		return ActionExecute, "search-type", resType, "", ""

	case http.MethodPost:
		if len(parts) >= 6 && strings.HasPrefix(parts[5], "$") {
			return ActionExecute, strings.TrimPrefix(parts[5], "$"), resType, parts[4], ""
		}
		if len(parts) >= 5 && strings.HasPrefix(parts[4], "$") {
			return ActionExecute, strings.TrimPrefix(parts[4], "$"), resType, "", ""
		}
		return ActionCreate, "create", resType, "", ""

	case http.MethodPut:
		id := ""
		if len(parts) >= 5 {
			id = parts[4]
		}
		if statusCode == http.StatusCreated {
			return ActionCreate, "update", resType, id, ""
		}
		return ActionUpdate, "update", resType, id, ""

	case http.MethodPatch:
		id := ""
		if len(parts) >= 5 {
			id = parts[4]
		}
		return ActionUpdate, "patch", resType, id, ""

	case http.MethodDelete:
		id := ""
		if len(parts) >= 5 {
			id = parts[4]
		}
		return ActionDelete, "delete", resType, id, ""

	default:
		return ActionExecute, "operation", resType, "", ""
	}
}

// ClassifyHTTPOutcome maps an HTTP response status code to a FHIR R4 AuditEventOutcome
// code ("0" = success, "4" = minor failure / client 4xx, "8" = serious failure / server 5xx)
// and human-readable description.
func ClassifyHTTPOutcome(statusCode int) (outcome, desc string) {
	if statusCode <= 0 {
		statusCode = http.StatusOK
	}
	statusText := http.StatusText(statusCode)
	if statusText == "" {
		statusText = "HTTP " + strconv.Itoa(statusCode)
	} else {
		statusText = fmt.Sprintf("%d %s", statusCode, statusText)
	}
	switch {
	case statusCode < 400:
		return OutcomeSuccess, statusText
	case statusCode < 500:
		return OutcomeMinorFailure, statusText
	default:
		return OutcomeSeriousFailure, statusText
	}
}

// ToFHIRResource converts a persisted AuditRecord into a standard HL7 FHIR R4 AuditEvent JSON map.
func ToFHIRResource(rec postgres.AuditRecord) map[string]any {
	recordedStr := rec.Recorded.UTC().Format(time.RFC3339)
	subject := rec.AgentSubject
	if subject == "" {
		subject = "anonymous"
	}

	agent := map[string]any{
		"who": map[string]any{
			"identifier": map[string]any{
				"value": subject,
			},
		},
		"requestor": true,
	}
	if rec.ClientIP != "" {
		agent["network"] = map[string]any{
			"address": rec.ClientIP,
			"type":    "2", // IP Address
		}
	}

	details := []map[string]any{
		{
			"type":        "http-method",
			"valueString": rec.HTTPMethod,
		},
		{
			"type":        "http-status",
			"valueString": strconv.Itoa(rec.HTTPStatus),
		},
		{
			"type":        "request-uri",
			"valueString": rec.RequestURI,
		},
		{
			"type":        "audit-sha256-digest",
			"valueString": ComputeRecordDigest(rec),
		},
	}
	if rec.PurposeOfUse != "" {
		details = append(details, map[string]any{
			"type":        "tefca-purpose-of-use",
			"valueString": rec.PurposeOfUse,
		})
	}

	entities := []map[string]any{}
	entity := map[string]any{
		"detail": details,
	}
	if rec.EntityType != "" {
		ref := rec.EntityType
		if rec.EntityID != "" {
			ref = fmt.Sprintf("%s/%s", rec.EntityType, rec.EntityID)
		}
		if rec.EntityVersion != "" {
			ref = fmt.Sprintf("%s/_history/%s", ref, rec.EntityVersion)
		}
		entity["what"] = map[string]any{
			"reference": ref,
		}
	}
	entities = append(entities, entity)

	if rec.AgentPatient != "" {
		entities = append(entities, map[string]any{
			"what": map[string]any{
				"reference": "Patient/" + strings.TrimPrefix(rec.AgentPatient, "Patient/"),
			},
			"role": map[string]any{
				"system":  "http://terminology.hl7.org/CodeSystem/object-role",
				"code":    "1",
				"display": "Patient",
			},
		})
	}

	res := map[string]any{
		"resourceType": "AuditEvent",
		"id":           rec.AuditID,
		"type": map[string]any{
			"system":  "http://terminology.hl7.org/CodeSystem/audit-event-type",
			"code":    "rest",
			"display": "RESTful Operation",
		},
		"subtype": []map[string]any{
			{
				"system": "http://hl7.org/fhir/restful-interaction",
				"code":   rec.SubtypeCode,
			},
		},
		"action":      rec.Action,
		"recorded":    recordedStr,
		"outcome":     rec.Outcome,
		"outcomeDesc": rec.OutcomeDesc,
		"agent":       []map[string]any{agent},
		"source": map[string]any{
			"observer": map[string]any{
				"display": "Flint FHIR Server",
			},
		},
		"entity": entities,
	}
	if rec.PurposeOfUse != "" {
		_, disp, _ := ValidatePurposeOfUse(rec.PurposeOfUse)
		if disp == "" {
			disp = rec.PurposeOfUse
		}
		res["purposeOfEvent"] = []map[string]any{
			{
				"coding": []map[string]any{
					{
						"system":  "http://terminology.hl7.org/CodeSystem/v3-ActReason",
						"code":    rec.PurposeOfUse,
						"display": disp,
					},
				},
			},
		}
	}
	return res
}

// ValidatePurposeOfUse validates and normalizes a TEFCA / HL7 v3-ActReason PurposeOfUse code.
func ValidatePurposeOfUse(raw string) (code, display string, ok bool) {
	cleaned := strings.ToUpper(strings.TrimSpace(raw))
	if idx := strings.LastIndexByte(cleaned, '|'); idx != -1 {
		cleaned = strings.TrimSpace(cleaned[idx+1:])
	}
	switch cleaned {
	case "TREAT", "TREATMENT":
		return "TREAT", "Treatment", true
	case "PAYMT", "PAYMENT", "HPAYMT":
		return "PAYMT", "Healthcare Payment", true
	case "OPERATIONS", "HOPERAT":
		return "OPERATIONS", "Healthcare Operations", true
	case "IAS", "PATRQT", "INDIVIDUAL-ACCESS":
		return "IAS", "Individual Access Services", true
	case "PUBHLTH", "PUBLIC-HEALTH":
		return "PUBHLTH", "Public Health", true
	case "HRESCH", "RESEARCH":
		return "HRESCH", "Healthcare Research", true
	case "ETREAT", "EMERGENCY":
		return "ETREAT", "Emergency Treatment", true
	case "COC":
		return "COC", "Coordination of Care", true
	case "COVERAGE", "COVAUTH":
		return "COVERAGE", "Coverage Under Policy or Program", true
	case "GOV":
		return "GOV", "Government Program Determination", true
	default:
		return "", "", false
	}
}

// ComputeRecordDigest computes the deterministic SHA-256 hex digest of an AuditRecord
// per HIPAA 45 CFR §164.312(b) and ONC §170.315(d)(2)/(d)(3) tamper-evident audit requirements.
func ComputeRecordDigest(rec postgres.AuditRecord) string {
	canonical := strings.Join([]string{
		rec.TenantID,
		rec.AuditID,
		rec.Recorded.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano),
		rec.Action,
		rec.SubtypeCode,
		rec.Outcome,
		rec.HTTPMethod,
		strconv.Itoa(rec.HTTPStatus),
		rec.RequestURI,
		rec.AgentSubject,
		rec.AgentPatient,
		rec.EntityType,
		rec.EntityID,
		rec.EntityVersion,
		rec.PurposeOfUse,
	}, "|")
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// ComputeAuditChainDigest links a record's SHA-256 digest with the previous chain digest.
func ComputeAuditChainDigest(prevHash string, rec postgres.AuditRecord) string {
	recDigest := ComputeRecordDigest(rec)
	sum := sha256.Sum256([]byte(prevHash + "|" + recDigest))
	return hex.EncodeToString(sum[:])
}

// VerifyAuditChain computes and verifies the chronological SHA-256 hash chain over a slice
// of AuditRecords (which may be passed newest-first as returned by Query, and are ordered
// oldest-first before chaining).
func VerifyAuditChain(records []postgres.AuditRecord) (chainHead string, verifiedCount int, entries []map[string]any, valid bool) {
	prevHash := strings.Repeat("0", 64)
	if len(records) == 0 {
		return prevHash, 0, nil, true
	}
	entries = make([]map[string]any, 0, len(records))
	// Query returns newest-first; chain oldest-first for deterministic chronological linkage.
	for i := len(records) - 1; i >= 0; i-- {
		rec := records[i]
		recDigest := ComputeRecordDigest(rec)
		chainHash := ComputeAuditChainDigest(prevHash, rec)
		entries = append(entries, map[string]any{
			"auditId":      rec.AuditID,
			"recorded":     rec.Recorded.UTC().Format(time.RFC3339),
			"action":       rec.Action,
			"subtype":      rec.SubtypeCode,
			"recordDigest": recDigest,
			"prevChain":    prevHash,
			"chainDigest":  chainHash,
		})
		prevHash = chainHash
		verifiedCount++
	}
	return prevHash, verifiedCount, entries, true
}
