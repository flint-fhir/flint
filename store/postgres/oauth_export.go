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

func hashToken(rawToken string) string {
	sum := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(sum[:])
}

// SaveAuthCode stores an OAuth2 authorization code (hashed with SHA-256) in PostgreSQL.
func (s *Store) SaveAuthCode(ctx context.Context, rawCode string, rec OAuthAuthCodeRecord) error {
	if s == nil || s.db == nil || rawCode == "" {
		return nil
	}
	_, _ = s.db.ExecContext(ctx, "DELETE FROM fhir_oauth_auth_code WHERE expires_at <= NOW()")
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO fhir_oauth_auth_code (
			code_hash, client_id, redirect_uri, scope, code_challenge, code_challenge_method,
			patient_id, encounter_id, tenant_id, nonce, fhir_user, extra_claims_text, expires_at, created_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, NOW())
		ON CONFLICT (code_hash) DO UPDATE SET
			client_id             = EXCLUDED.client_id,
			redirect_uri          = EXCLUDED.redirect_uri,
			scope                 = EXCLUDED.scope,
			code_challenge        = EXCLUDED.code_challenge,
			code_challenge_method = EXCLUDED.code_challenge_method,
			patient_id            = EXCLUDED.patient_id,
			encounter_id          = EXCLUDED.encounter_id,
			tenant_id             = EXCLUDED.tenant_id,
			nonce                 = EXCLUDED.nonce,
			fhir_user             = EXCLUDED.fhir_user,
			extra_claims_text     = EXCLUDED.extra_claims_text,
			expires_at            = EXCLUDED.expires_at
	`, hashToken(rawCode), rec.ClientID, rec.RedirectURI, rec.Scope, rec.CodeChallenge, rec.CodeChallengeMethod,
		rec.PatientID, rec.EncounterID, rec.TenantID, rec.Nonce, rec.FhirUser, rec.ExtraClaimsJSON, rec.ExpiresAt.UTC())
	return err
}

// ConsumeAuthCode atomically deletes and returns an unexpired OAuth2 authorization code record.
func (s *Store) ConsumeAuthCode(ctx context.Context, rawCode string) (OAuthAuthCodeRecord, bool, error) {
	if s == nil || s.db == nil || rawCode == "" {
		return OAuthAuthCodeRecord{}, false, nil
	}
	var rec OAuthAuthCodeRecord
	err := s.db.QueryRowContext(ctx, `
		DELETE FROM fhir_oauth_auth_code
		WHERE code_hash = $1 AND expires_at > NOW()
		RETURNING client_id, redirect_uri, scope, code_challenge, code_challenge_method,
		          patient_id, encounter_id, tenant_id, nonce, fhir_user, extra_claims_text, expires_at
	`, hashToken(rawCode)).Scan(
		&rec.ClientID, &rec.RedirectURI, &rec.Scope, &rec.CodeChallenge, &rec.CodeChallengeMethod,
		&rec.PatientID, &rec.EncounterID, &rec.TenantID, &rec.Nonce, &rec.FhirUser, &rec.ExtraClaimsJSON, &rec.ExpiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthAuthCodeRecord{}, false, nil
	}
	if err != nil {
		return OAuthAuthCodeRecord{}, false, err
	}
	return rec, true, nil
}

// SaveRefreshToken stores an OAuth2 refresh token (hashed with SHA-256) in PostgreSQL.
func (s *Store) SaveRefreshToken(ctx context.Context, rawToken string, rec OAuthRefreshTokenRecord) error {
	if s == nil || s.db == nil || rawToken == "" {
		return nil
	}
	_, _ = s.db.ExecContext(ctx, "DELETE FROM fhir_oauth_refresh_token WHERE expires_at <= NOW()")
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO fhir_oauth_refresh_token (
			token_hash, subject, grant_id, client_id, tenant_id, patient_id, encounter_id,
			scope, nonce, fhir_user, extra_claims_text, expires_at, created_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, NOW())
		ON CONFLICT (token_hash) DO UPDATE SET
			subject           = EXCLUDED.subject,
			grant_id          = EXCLUDED.grant_id,
			client_id         = EXCLUDED.client_id,
			tenant_id         = EXCLUDED.tenant_id,
			patient_id        = EXCLUDED.patient_id,
			encounter_id      = EXCLUDED.encounter_id,
			scope             = EXCLUDED.scope,
			nonce             = EXCLUDED.nonce,
			fhir_user         = EXCLUDED.fhir_user,
			extra_claims_text = EXCLUDED.extra_claims_text,
			expires_at        = EXCLUDED.expires_at
	`, hashToken(rawToken), rec.Subject, rec.GrantID, rec.ClientID, rec.TenantID,
		rec.PatientID, rec.EncounterID, rec.Scope, rec.Nonce, rec.FhirUser, rec.ExtraClaimsJSON, rec.ExpiresAt.UTC())
	return err
}

// PeekRefreshToken reads an unexpired OAuth2 refresh token record without consuming it.
func (s *Store) PeekRefreshToken(ctx context.Context, rawToken string) (OAuthRefreshTokenRecord, bool, error) {
	if s == nil || s.db == nil || rawToken == "" {
		return OAuthRefreshTokenRecord{}, false, nil
	}
	var rec OAuthRefreshTokenRecord
	err := s.db.QueryRowContext(ctx, `
		SELECT subject, grant_id, client_id, tenant_id, patient_id, encounter_id,
		       scope, nonce, fhir_user, extra_claims_text, expires_at
		FROM fhir_oauth_refresh_token
		WHERE token_hash = $1 AND expires_at > NOW()
	`, hashToken(rawToken)).Scan(
		&rec.Subject, &rec.GrantID, &rec.ClientID, &rec.TenantID, &rec.PatientID, &rec.EncounterID,
		&rec.Scope, &rec.Nonce, &rec.FhirUser, &rec.ExtraClaimsJSON, &rec.ExpiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthRefreshTokenRecord{}, false, nil
	}
	if err != nil {
		return OAuthRefreshTokenRecord{}, false, err
	}
	return rec, true, nil
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
		RETURNING subject, grant_id, client_id, tenant_id, patient_id, encounter_id,
		          scope, nonce, fhir_user, extra_claims_text, expires_at
	`, hashToken(rawToken)).Scan(
		&rec.Subject, &rec.GrantID, &rec.ClientID, &rec.TenantID, &rec.PatientID, &rec.EncounterID,
		&rec.Scope, &rec.Nonce, &rec.FhirUser, &rec.ExtraClaimsJSON, &rec.ExpiresAt,
	)
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

// ClaimNonce atomically claims a single-use cryptographic replay nonce in PostgreSQL.
// Returns (true, nil) if freshly claimed, or (false, nil) if already claimed and unexpired.
func (s *Store) ClaimNonce(ctx context.Context, nonceKey string, ttl time.Duration) (bool, error) {
	if s == nil || s.db == nil || nonceKey == "" {
		return false, nil
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	h := hashToken("nonce:" + nonceKey)
	_, _ = s.db.ExecContext(ctx, "DELETE FROM fhir_oauth_revoked_token WHERE token_hash = $1 AND expires_at <= NOW()", h)
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO fhir_oauth_revoked_token (token_hash, expires_at, revoked_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (token_hash) DO NOTHING
	`, h, time.Now().UTC().Add(ttl))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// GetOrCreateClusterSecret atomically retrieves or initializes a cluster-wide secret in PostgreSQL.
func (s *Store) GetOrCreateClusterSecret(ctx context.Context, key string, generate func() ([]byte, error)) ([]byte, error) {
	if s == nil || s.db == nil || key == "" {
		if generate != nil {
			return generate()
		}
		return nil, nil
	}
	var existing []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT secret_bytes FROM fhir_cluster_secret WHERE secret_key = $1
	`, key).Scan(&existing)
	if err == nil && len(existing) > 0 {
		return append([]byte(nil), existing...), nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if generate == nil {
		return nil, sql.ErrNoRows
	}
	genBytes, err := generate()
	if err != nil {
		return nil, err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO fhir_cluster_secret (secret_key, secret_bytes, created_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (secret_key) DO NOTHING
	`, key, genBytes)
	if err != nil {
		return nil, err
	}
	err = s.db.QueryRowContext(ctx, `
		SELECT secret_bytes FROM fhir_cluster_secret WHERE secret_key = $1
	`, key).Scan(&existing)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), existing...), nil
}

// PutClusterState upserts a versioned cluster state entry in PostgreSQL.
func (s *Store) PutClusterState(ctx context.Context, rec ClusterStateRecord) (ClusterStateRecord, error) {
	if s == nil || s.db == nil || rec.Namespace == "" || rec.Key == "" {
		return rec, nil
	}
	valBlob := rec.Value
	if valBlob == nil {
		valBlob = []byte{}
	}
	var expArg any
	if rec.ExpiresAt != nil && !rec.ExpiresAt.IsZero() {
		expArg = rec.ExpiresAt.UTC()
	}
	out := rec
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO fhir_cluster_state (
			namespace, state_key, tenant_id, secondary_key, value_blob, version, expires_at, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, 1, $6, NOW())
		ON CONFLICT (namespace, state_key) DO UPDATE SET
			tenant_id     = EXCLUDED.tenant_id,
			secondary_key = EXCLUDED.secondary_key,
			value_blob    = EXCLUDED.value_blob,
			version       = fhir_cluster_state.version + 1,
			expires_at    = EXCLUDED.expires_at,
			updated_at    = NOW()
		RETURNING version, updated_at
	`, rec.Namespace, rec.Key, rec.TenantID, rec.SecondaryKey, valBlob, expArg).Scan(&out.Version, &out.UpdatedAt)
	if err != nil {
		return ClusterStateRecord{}, err
	}
	return out, nil
}

func scanClusterStateRow(scanner interface{ Scan(...any) error }) (*ClusterStateRecord, error) {
	var rec ClusterStateRecord
	var exp sql.NullTime
	if err := scanner.Scan(
		&rec.Namespace, &rec.Key, &rec.TenantID, &rec.SecondaryKey,
		&rec.Value, &rec.Version, &exp, &rec.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if exp.Valid {
		t := exp.Time.UTC()
		rec.ExpiresAt = &t
	}
	return &rec, nil
}

// GetClusterState retrieves an unexpired cluster state entry by (namespace, key).
func (s *Store) GetClusterState(ctx context.Context, namespace, key string) (*ClusterStateRecord, bool, error) {
	if s == nil || s.db == nil || namespace == "" || key == "" {
		return nil, false, nil
	}
	rec, err := scanClusterStateRow(s.db.QueryRowContext(ctx, `
		SELECT namespace, state_key, tenant_id, secondary_key, value_blob, version, expires_at, updated_at
		FROM fhir_cluster_state
		WHERE namespace = $1 AND state_key = $2 AND (expires_at IS NULL OR expires_at > NOW())
	`, namespace, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return rec, true, nil
}

// GetClusterStateBySecondary retrieves the latest unexpired cluster state entry by (namespace, secondaryKey).
func (s *Store) GetClusterStateBySecondary(ctx context.Context, namespace, secondaryKey string) (*ClusterStateRecord, bool, error) {
	if s == nil || s.db == nil || namespace == "" || secondaryKey == "" {
		return nil, false, nil
	}
	rec, err := scanClusterStateRow(s.db.QueryRowContext(ctx, `
		SELECT namespace, state_key, tenant_id, secondary_key, value_blob, version, expires_at, updated_at
		FROM fhir_cluster_state
		WHERE namespace = $1 AND secondary_key = $2 AND (expires_at IS NULL OR expires_at > NOW())
		ORDER BY updated_at DESC, state_key ASC
		LIMIT 1
	`, namespace, secondaryKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return rec, true, nil
}

// CompareAndSwapClusterState atomically inserts (when expectedVersion == 0) or updates
// (when current version == expectedVersion) an unexpired cluster state entry.
func (s *Store) CompareAndSwapClusterState(ctx context.Context, expectedVersion int64, rec ClusterStateRecord) (ClusterStateRecord, bool, error) {
	if s == nil || s.db == nil || rec.Namespace == "" || rec.Key == "" {
		return ClusterStateRecord{}, false, nil
	}
	valBlob := rec.Value
	if valBlob == nil {
		valBlob = []byte{}
	}
	var expArg any
	if rec.ExpiresAt != nil && !rec.ExpiresAt.IsZero() {
		expArg = rec.ExpiresAt.UTC()
	}
	_, _ = s.db.ExecContext(ctx, `
		DELETE FROM fhir_cluster_state
		WHERE namespace = $1 AND state_key = $2 AND expires_at IS NOT NULL AND expires_at <= NOW()
	`, rec.Namespace, rec.Key)

	out := rec
	var err error
	if expectedVersion == 0 {
		err = s.db.QueryRowContext(ctx, `
			INSERT INTO fhir_cluster_state (
				namespace, state_key, tenant_id, secondary_key, value_blob, version, expires_at, updated_at
			)
			VALUES ($1, $2, $3, $4, $5, 1, $6, NOW())
			ON CONFLICT (namespace, state_key) DO NOTHING
			RETURNING version, updated_at
		`, rec.Namespace, rec.Key, rec.TenantID, rec.SecondaryKey, valBlob, expArg).Scan(&out.Version, &out.UpdatedAt)
	} else {
		err = s.db.QueryRowContext(ctx, `
			UPDATE fhir_cluster_state
			SET tenant_id     = $3,
			    secondary_key = $4,
			    value_blob    = $5,
			    version       = version + 1,
			    expires_at    = $6,
			    updated_at    = NOW()
			WHERE namespace = $1 AND state_key = $2 AND version = $7
			  AND (expires_at IS NULL OR expires_at > NOW())
			RETURNING version, updated_at
		`, rec.Namespace, rec.Key, rec.TenantID, rec.SecondaryKey, valBlob, expArg, expectedVersion).Scan(&out.Version, &out.UpdatedAt)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ClusterStateRecord{}, false, nil
	}
	if err != nil {
		return ClusterStateRecord{}, false, err
	}
	return out, true, nil
}

// ConsumeClusterState atomically deletes and returns an unexpired cluster state entry.
func (s *Store) ConsumeClusterState(ctx context.Context, namespace, key string) (*ClusterStateRecord, bool, error) {
	if s == nil || s.db == nil || namespace == "" || key == "" {
		return nil, false, nil
	}
	rec, err := scanClusterStateRow(s.db.QueryRowContext(ctx, `
		DELETE FROM fhir_cluster_state
		WHERE namespace = $1 AND state_key = $2 AND (expires_at IS NULL OR expires_at > NOW())
		RETURNING namespace, state_key, tenant_id, secondary_key, value_blob, version, expires_at, updated_at
	`, namespace, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return rec, true, nil
}

// DeleteClusterState deletes a cluster state entry by (namespace, key).
func (s *Store) DeleteClusterState(ctx context.Context, namespace, key string) (bool, error) {
	if s == nil || s.db == nil || namespace == "" || key == "" {
		return false, nil
	}
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM fhir_cluster_state WHERE namespace = $1 AND state_key = $2
	`, namespace, key)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListClusterStates lists unexpired cluster state entries in namespace (optionally filtered by tenantID).
func (s *Store) ListClusterStates(ctx context.Context, namespace, tenantID string) ([]ClusterStateRecord, error) {
	if s == nil || s.db == nil || namespace == "" {
		return nil, nil
	}
	var (
		rows *sql.Rows
		err  error
	)
	if tenantID != "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT namespace, state_key, tenant_id, secondary_key, value_blob, version, expires_at, updated_at
			FROM fhir_cluster_state
			WHERE namespace = $1 AND tenant_id = $2 AND (expires_at IS NULL OR expires_at > NOW())
			ORDER BY state_key ASC
		`, namespace, tenantID)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT namespace, state_key, tenant_id, secondary_key, value_blob, version, expires_at, updated_at
			FROM fhir_cluster_state
			WHERE namespace = $1 AND (expires_at IS NULL OR expires_at > NOW())
			ORDER BY state_key ASC
		`, namespace)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ClusterStateRecord
	for rows.Next() {
		rec, err := scanClusterStateRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	return out, rows.Err()
}

// SaveExportJob persists a Bulk Data ($export) job in PostgreSQL.
func (s *Store) SaveExportJob(ctx context.Context, job ExportJobRecord) error {
	if s == nil || s.db == nil || job.ID == "" {
		return nil
	}
	status := job.Status
	if status == "" {
		status = "complete"
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

	return s.withTenantTx(ctx, job.TenantID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO fhir_export_job (
				job_id, tenant_id, patient_id, status, err_msg, output_format, cancel_requested,
				request_url, created_at, types_order, counts, files_json, combined
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			ON CONFLICT (job_id) DO UPDATE SET
				tenant_id        = EXCLUDED.tenant_id,
				patient_id       = EXCLUDED.patient_id,
				status           = EXCLUDED.status,
				err_msg          = EXCLUDED.err_msg,
				output_format    = EXCLUDED.output_format,
				cancel_requested = EXCLUDED.cancel_requested,
				request_url      = EXCLUDED.request_url,
				types_order      = EXCLUDED.types_order,
				counts           = EXCLUDED.counts,
				files_json       = EXCLUDED.files_json,
				combined         = EXCLUDED.combined
		`, job.ID, job.TenantID, job.PatientID, status, job.Err, job.OutputFormat, job.CancelRequested,
			job.RequestURL, job.CreatedAt.UTC(), string(typesJSON), string(countsJSON), string(filesJSON), combinedBytes)
		return err
	})
}

// GetExportJob retrieves a persisted Bulk Data ($export) job by tenant and job ID.
func (s *Store) GetExportJob(ctx context.Context, tenantID, jobID string) (*ExportJobRecord, error) {
	if s == nil || s.db == nil || jobID == "" {
		return nil, sql.ErrNoRows
	}
	var rec ExportJobRecord
	var typesRaw, countsRaw, filesRaw string
	err := s.withTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT job_id, tenant_id, patient_id, status, err_msg, output_format, cancel_requested,
			       request_url, created_at, types_order, counts, files_json, combined
			FROM fhir_export_job
			WHERE tenant_id = $1 AND job_id = $2
		`, tenantID, jobID).Scan(
			&rec.ID, &rec.TenantID, &rec.PatientID, &rec.Status, &rec.Err, &rec.OutputFormat, &rec.CancelRequested,
			&rec.RequestURL, &rec.CreatedAt, &typesRaw, &countsRaw, &filesRaw, &rec.Combined,
		)
	})
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
	var n int64
	err := s.withTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			DELETE FROM fhir_export_job WHERE tenant_id = $1 AND job_id = $2
		`, tenantID, jobID)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
