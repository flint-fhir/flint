package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// TenantIsolationMode configures how multi-tenant FHIR data is isolated in PostgreSQL.
type TenantIsolationMode string

const (
	// TenantIsolationShared (default) stores all tenants in shared public tables
	// keyed by a leading tenant_id column in every primary key and covering B-tree index.
	TenantIsolationShared TenantIsolationMode = "shared"

	// TenantIsolationSchema provisions a dedicated PostgreSQL schema (tenant_<id>) per tenant
	// cloned from the public template tables and scopes all tenant queries and transactions
	// via SET LOCAL search_path TO "tenant_<id>", public.
	TenantIsolationSchema TenantIsolationMode = "schema"
)

// ParseTenantIsolationMode parses a configuration string (e.g. from FLINT_TENANT_ISOLATION
// or --tenant-isolation) into a TenantIsolationMode. Empty string defaults to TenantIsolationShared.
func ParseTenantIsolationMode(raw string) (TenantIsolationMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "shared", "public", "table":
		return TenantIsolationShared, nil
	case "schema", "schema-per-tenant", "per-tenant", "tenant-schema":
		return TenantIsolationSchema, nil
	default:
		return "", fmt.Errorf("invalid tenant isolation mode %q (valid modes: %q, %q)", raw, TenantIsolationShared, TenantIsolationSchema)
	}
}

// StoreOption configures a Postgres Store instance.
type StoreOption func(*Store)

// WithTenantIsolation configures the Store's multi-tenant storage isolation mode.
func WithTenantIsolation(mode TenantIsolationMode) StoreOption {
	return func(s *Store) {
		if mode == TenantIsolationSchema {
			s.isolationMode = TenantIsolationSchema
		} else {
			s.isolationMode = TenantIsolationShared
		}
	}
}

// SetTenantIsolation updates the Store's multi-tenant storage isolation mode.
func (s *Store) SetTenantIsolation(mode TenantIsolationMode) {
	if mode == TenantIsolationSchema {
		s.isolationMode = TenantIsolationSchema
	} else {
		s.isolationMode = TenantIsolationShared
	}
}

// TenantIsolation returns the Store's configured multi-tenant storage isolation mode.
func (s *Store) TenantIsolation() TenantIsolationMode {
	if s == nil || s.isolationMode == "" {
		return TenantIsolationShared
	}
	return s.isolationMode
}

// TenantSchemaName deterministically converts a tenant identifier into a safe PostgreSQL
// schema name (e.g. "hca-west" -> "tenant_hca_west").
func TenantSchemaName(tenantID string) (string, error) {
	raw := strings.ToLower(strings.TrimSpace(tenantID))
	if raw == "" {
		return "", fmt.Errorf("tenant ID is empty")
	}
	var b strings.Builder
	prevUnderscore := false
	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') {
			b.WriteByte(ch)
			prevUnderscore = false
		} else if !prevUnderscore && b.Len() > 0 {
			b.WriteByte('_')
			prevUnderscore = true
		}
	}
	sanitized := strings.Trim(b.String(), "_")
	if sanitized == "" {
		return "", fmt.Errorf("invalid tenant ID %q", tenantID)
	}
	return "tenant_" + sanitized, nil
}

func quotePostgresIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

// ProvisionTenantSchema ensures the dedicated PostgreSQL schema and cloned tables exist
// for tenantID, caching provisioned schemas in memory to avoid repeated DDL checks.
func (s *Store) ProvisionTenantSchema(ctx context.Context, tenantID string) (string, error) {
	schemaName, err := TenantSchemaName(tenantID)
	if err != nil {
		return "", err
	}
	if _, ok := s.provisionedTenants.Load(schemaName); ok {
		return schemaName, nil
	}
	if s.db == nil {
		return schemaName, nil
	}
	var provisioned string
	if err := s.db.QueryRowContext(ctx, "SELECT provision_tenant($1)", tenantID).Scan(&provisioned); err != nil {
		return "", fmt.Errorf("provision tenant schema %s: %w", schemaName, err)
	}
	s.provisionedTenants.Store(schemaName, true)
	return schemaName, nil
}

func (s *Store) beginTenantTx(ctx context.Context, tenantID string) (*sql.Tx, error) {
	if s.TenantIsolation() == TenantIsolationSchema && strings.TrimSpace(tenantID) != "" {
		schemaName, err := s.ProvisionTenantSchema(ctx, tenantID)
		if err != nil {
			return nil, err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, fmt.Errorf("begin tx: %w", err)
		}
		setPathSQL := fmt.Sprintf("SET LOCAL search_path TO %s, public", quotePostgresIdent(schemaName))
		if _, err := tx.ExecContext(ctx, setPathSQL); err != nil {
			_ = tx.Rollback()
			return nil, fmt.Errorf("set tenant search_path %s: %w", schemaName, err)
		}
		return tx, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	return tx, nil
}

func (s *Store) withTenantExec(ctx context.Context, tenantID string, fn func(exec queryExecer) error) error {
	if s.TenantIsolation() != TenantIsolationSchema || strings.TrimSpace(tenantID) == "" {
		return fn(s.db)
	}
	tx, err := s.beginTenantTx(ctx, tenantID)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
