package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// AuditRecord represents a persisted HIPAA / ONC security audit event (FHIR R4 AuditEvent).
type AuditRecord struct {
	TenantID      string
	AuditID       string
	Recorded      time.Time
	Action        string // C, R, U, D, E
	SubtypeCode   string // read, vread, update, delete, history-instance, create, search-type, validate, batch
	Outcome       string // 0 (success), 4 (minor failure / 4xx), 8 (serious failure / 5xx)
	OutcomeDesc   string
	HTTPMethod    string
	HTTPStatus    int
	RequestURI    string
	AgentSubject  string
	AgentPatient  string
	ClientIP      string
	EntityType    string
	EntityID      string
	EntityVersion string
}

// AuditQueryParams specifies search filters for querying FHIR AuditEvent records.
type AuditQueryParams struct {
	TenantID     string
	Action       string
	SubtypeCode  string
	Outcome      string
	AgentSubject string
	AgentPatient string
	EntityType   string
	EntityID     string
	Count        int
	Offset       int
}

// WriteAuditEvent inserts an immutable security audit record into fhir_audit_event.
func (s *Store) WriteAuditEvent(ctx context.Context, rec AuditRecord) error {
	if rec.Recorded.IsZero() {
		rec.Recorded = time.Now().UTC()
	}
	if rec.AgentSubject == "" {
		rec.AgentSubject = "anonymous"
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO fhir_audit_event (
			tenant_id, audit_id, recorded, action, subtype_code, outcome, outcome_desc,
			http_method, http_status, request_uri, agent_subject, agent_patient,
			client_ip, entity_type, entity_id, entity_version
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7,
			$8, $9, $10, $11, $12,
			$13, $14, $15, $16
		)
		ON CONFLICT (tenant_id, audit_id) DO NOTHING
	`,
		rec.TenantID, rec.AuditID, rec.Recorded, rec.Action, rec.SubtypeCode, rec.Outcome, rec.OutcomeDesc,
		rec.HTTPMethod, rec.HTTPStatus, rec.RequestURI, rec.AgentSubject, rec.AgentPatient,
		rec.ClientIP, rec.EntityType, rec.EntityID, rec.EntityVersion,
	)
	if err != nil {
		return fmt.Errorf("insert audit event: %w", err)
	}
	return nil
}

// ReadAuditEvent reads a single AuditRecord by (tenant_id, audit_id).
func (s *Store) ReadAuditEvent(ctx context.Context, tenantID, auditID string) (*AuditRecord, error) {
	var rec AuditRecord
	err := s.db.QueryRowContext(ctx, `
		SELECT
			tenant_id, audit_id, recorded, action, subtype_code, outcome, outcome_desc,
			http_method, http_status, request_uri, agent_subject, agent_patient,
			client_ip, entity_type, entity_id, entity_version
		FROM fhir_audit_event
		WHERE tenant_id = $1 AND audit_id = $2
	`, tenantID, auditID).Scan(
		&rec.TenantID, &rec.AuditID, &rec.Recorded, &rec.Action, &rec.SubtypeCode, &rec.Outcome, &rec.OutcomeDesc,
		&rec.HTTPMethod, &rec.HTTPStatus, &rec.RequestURI, &rec.AgentSubject, &rec.AgentPatient,
		&rec.ClientIP, &rec.EntityType, &rec.EntityID, &rec.EntityVersion,
	)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// QueryAuditEvents searches fhir_audit_event ordered by recorded descending.
func (s *Store) QueryAuditEvents(ctx context.Context, params AuditQueryParams) ([]AuditRecord, int, error) {
	count := params.Count
	if count <= 0 {
		count = 100
	}
	offset := max(params.Offset, 0)

	var clauses []string
	var args []any
	addFilter := func(col, val string) {
		if val == "" {
			return
		}
		args = append(args, val)
		clauses = append(clauses, fmt.Sprintf("%s = $%d", col, len(args)))
	}

	addFilter("tenant_id", params.TenantID)
	addFilter("action", params.Action)
	addFilter("subtype_code", params.SubtypeCode)
	addFilter("outcome", params.Outcome)
	addFilter("agent_subject", params.AgentSubject)
	addFilter("agent_patient", params.AgentPatient)
	addFilter("entity_type", params.EntityType)
	addFilter("entity_id", params.EntityID)

	whereSQL := ""
	if len(clauses) > 0 {
		whereSQL = "WHERE " + strings.Join(clauses, " AND ")
	}

	var total int
	countQuery := "SELECT COUNT(*) FROM fhir_audit_event " + whereSQL
	if err := s.db.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count audit events: %w", err)
	}

	limitPos := len(args) + 1
	offsetPos := len(args) + 2
	queryArgs := append(append([]any{}, args...), count, offset)
	selectQuery := fmt.Sprintf(`
		SELECT
			tenant_id, audit_id, recorded, action, subtype_code, outcome, outcome_desc,
			http_method, http_status, request_uri, agent_subject, agent_patient,
			client_ip, entity_type, entity_id, entity_version
		FROM fhir_audit_event
		%s
		ORDER BY recorded DESC, audit_id DESC
		LIMIT $%d OFFSET $%d
	`, whereSQL, limitPos, offsetPos)

	rows, err := s.db.QueryContext(ctx, selectQuery, queryArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("query audit events: %w", err)
	}
	defer rows.Close()

	var records []AuditRecord
	for rows.Next() {
		var rec AuditRecord
		if err := rows.Scan(
			&rec.TenantID, &rec.AuditID, &rec.Recorded, &rec.Action, &rec.SubtypeCode, &rec.Outcome, &rec.OutcomeDesc,
			&rec.HTTPMethod, &rec.HTTPStatus, &rec.RequestURI, &rec.AgentSubject, &rec.AgentPatient,
			&rec.ClientIP, &rec.EntityType, &rec.EntityID, &rec.EntityVersion,
		); err != nil {
			return nil, 0, fmt.Errorf("scan audit event: %w", err)
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return records, total, nil
}
