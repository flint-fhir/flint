package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// OAuthRefreshTokenRecord represents a persisted OAuth2 refresh token grant.
type OAuthRefreshTokenRecord struct {
	Subject     string
	PatientID   string
	EncounterID string
	Scope       string
	ExpiresAt   time.Time
}

// ExportJobRecord represents a persisted FHIR Bulk Data ($export) job.
type ExportJobRecord struct {
	ID         string
	TenantID   string
	PatientID  string
	CreatedAt  time.Time
	RequestURL string
	TypesOrder []string
	Files      map[string][]byte
	Counts     map[string]int
	Combined   []byte
}

func hashToken(rawToken string) string {
	sum := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(sum[:])
}

// SaveRefreshToken stores an OAuth2 refresh token (hashed with SHA-256) in PostgreSQL.
func (s *Store) SaveRefreshToken(ctx context.Context, rawToken string, rec OAuthRefreshTokenRecord) error {
	if s == nil || s.db == nil || rawToken == "" {
		return nil
	}
	_, _ = s.db.ExecContext(ctx, "DELETE FROM fhir_oauth_refresh_token WHERE expires_at <= NOW()")
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO fhir_oauth_refresh_token (token_hash, subject, patient_id, encounter_id, scope, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
		ON CONFLICT (token_hash) DO UPDATE SET
			subject      = EXCLUDED.subject,
			patient_id   = EXCLUDED.patient_id,
			encounter_id = EXCLUDED.encounter_id,
			scope        = EXCLUDED.scope,
			expires_at   = EXCLUDED.expires_at
	`, hashToken(rawToken), rec.Subject, rec.PatientID, rec.EncounterID, rec.Scope, rec.ExpiresAt.UTC())
	return err
}

// ConsumeRefreshToken atomically deletes and returns an unexpired OAuth2 refresh token record.
func (s *Store) ConsumeRefreshToken(ctx context.Context, rawToken string) (OAuthRefreshTokenRecord, bool, error) {
	if s == nil || s.db == nil || rawToken == "" {
		return OAuthRefreshTokenRecord{}, false, nil
	}
	var rec OAuthRefreshTokenRecord
	err := s.db.QueryRowContext(ctx, `
		DELETE FROM fhir_oauth_refresh_token
		WHERE token_hash = $1 AND expires_at > NOW()
		RETURNING subject, patient_id, encounter_id, scope, expires_at
	`, hashToken(rawToken)).Scan(&rec.Subject, &rec.PatientID, &rec.EncounterID, &rec.Scope, &rec.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthRefreshTokenRecord{}, false, nil
	}
	if err != nil {
		return OAuthRefreshTokenRecord{}, false, err
	}
	return rec, true, nil
}

// RevokeToken records a token revocation in PostgreSQL and deletes any matching refresh token.
func (s *Store) RevokeToken(ctx context.Context, rawToken string, ttl time.Duration) error {
	if s == nil || s.db == nil || rawToken == "" {
		return nil
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	h := hashToken(rawToken)
	_, _ = s.db.ExecContext(ctx, "DELETE FROM fhir_oauth_refresh_token WHERE token_hash = $1", h)
	_, _ = s.db.ExecContext(ctx, "DELETE FROM fhir_oauth_revoked_token WHERE expires_at <= NOW()")
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO fhir_oauth_revoked_token (token_hash, expires_at, revoked_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (token_hash) DO UPDATE SET
			expires_at = EXCLUDED.expires_at,
			revoked_at = NOW()
	`, h, time.Now().UTC().Add(ttl))
	return err
}

// IsTokenRevoked checks whether rawToken has been revoked in PostgreSQL.
func (s *Store) IsTokenRevoked(ctx context.Context, rawToken string) (bool, error) {
	if s == nil || s.db == nil || rawToken == "" {
		return false, nil
	}
	var exists int
	err := s.db.QueryRowContext(ctx, `
		SELECT 1 FROM fhir_oauth_revoked_token
		WHERE token_hash = $1 AND expires_at > NOW()
	`, hashToken(rawToken)).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// SaveExportJob persists a Bulk Data ($export) job in PostgreSQL.
func (s *Store) SaveExportJob(ctx context.Context, job ExportJobRecord) error {
	if s == nil || s.db == nil || job.ID == "" {
		return nil
	}
	typesJSON, err := json.Marshal(job.TypesOrder)
	if err != nil {
		return fmt.Errorf("marshal types_order: %w", err)
	}
	countsJSON, err := json.Marshal(job.Counts)
	if err != nil {
		return fmt.Errorf("marshal counts: %w", err)
	}
	encodedFiles := make(map[string]string, len(job.Files))
	for k, v := range job.Files {
		encodedFiles[k] = base64.StdEncoding.EncodeToString(v)
	}
	filesJSON, err := json.Marshal(encodedFiles)
	if err != nil {
		return fmt.Errorf("marshal files: %w", err)
	}

	combinedBytes := job.Combined
	if combinedBytes == nil {
		combinedBytes = []byte{}
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO fhir_export_job (job_id, tenant_id, patient_id, request_url, created_at, types_order, counts, files_json, combined)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (job_id) DO UPDATE SET
			tenant_id   = EXCLUDED.tenant_id,
			patient_id  = EXCLUDED.patient_id,
			request_url = EXCLUDED.request_url,
			types_order = EXCLUDED.types_order,
			counts      = EXCLUDED.counts,
			files_json  = EXCLUDED.files_json,
			combined    = EXCLUDED.combined
	`, job.ID, job.TenantID, job.PatientID, job.RequestURL, job.CreatedAt.UTC(), string(typesJSON), string(countsJSON), string(filesJSON), combinedBytes)
	return err
}

// GetExportJob retrieves a persisted Bulk Data ($export) job by tenant and job ID.
func (s *Store) GetExportJob(ctx context.Context, tenantID, jobID string) (*ExportJobRecord, error) {
	if s == nil || s.db == nil || jobID == "" {
		return nil, sql.ErrNoRows
	}
	var rec ExportJobRecord
	var typesRaw, countsRaw, filesRaw string
	err := s.db.QueryRowContext(ctx, `
		SELECT job_id, tenant_id, patient_id, request_url, created_at, types_order, counts, files_json, combined
		FROM fhir_export_job
		WHERE tenant_id = $1 AND job_id = $2
	`, tenantID, jobID).Scan(
		&rec.ID, &rec.TenantID, &rec.PatientID, &rec.RequestURL, &rec.CreatedAt,
		&typesRaw, &countsRaw, &filesRaw, &rec.Combined,
	)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(typesRaw), &rec.TypesOrder)
	_ = json.Unmarshal([]byte(countsRaw), &rec.Counts)
	var encodedFiles map[string]string
	if err := json.Unmarshal([]byte(filesRaw), &encodedFiles); err == nil {
		rec.Files = make(map[string][]byte, len(encodedFiles))
		for k, b64 := range encodedFiles {
			if decoded, dErr := base64.StdEncoding.DecodeString(b64); dErr == nil {
				rec.Files[k] = decoded
			}
		}
	}
	return &rec, nil
}

// DeleteExportJob deletes a Bulk Data ($export) job by tenant and job ID.
func (s *Store) DeleteExportJob(ctx context.Context, tenantID, jobID string) (bool, error) {
	if s == nil || s.db == nil || jobID == "" {
		return false, nil
	}
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM fhir_export_job WHERE tenant_id = $1 AND job_id = $2
	`, tenantID, jobID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
