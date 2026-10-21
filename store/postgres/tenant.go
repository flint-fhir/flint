package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
)

var (
	// ErrNoTenant is returned when a tenant-scoped database operation is invoked with an empty tenant ID.
	ErrNoTenant = errors.New("postgres: no tenant specified")

	// ErrInvalidTenantSlug is returned when a tenant slug fails identifier validation.
	ErrInvalidTenantSlug = errors.New("postgres: invalid tenant slug format")

	// ErrCrossTenantBatch is returned when a batch or bundle transaction spans multiple tenants.
	ErrCrossTenantBatch = errors.New("postgres: cross-tenant operations are not permitted in a single transaction")
)

const (
	tenantSchemaPrefix = "tenant_"
	maxTenantSlugLen   = 63 - len(tenantSchemaPrefix)
)

var validTenantSlug = regexp.MustCompile(`^[a-z0-9]([-_a-z0-9]*[a-z0-9])?$`)

// Option configures a Postgres Store.
type Option func(*Store)

// WithAutoProvision controls whether the Store automatically creates and migrates
// the per-tenant schema ("tenant_<slug>") on first access (default: true).
// Set to false when tenant schemas are strictly pre-provisioned by an external
// schema control plane (such as Azra's tenant-worker + Atlas Operator).
func WithAutoProvision(enabled bool) Option {
	return func(s *Store) {
		s.autoProvision = enabled
	}
}

// WithTenantRoleSwitch controls whether each tenant transaction also executes
// SET LOCAL ROLE "tenant_<slug>" alongside SET LOCAL search_path TO "tenant_<slug>"
// (default: false). Enable when running against a shared database where each
// tenant schema has a dedicated NOLOGIN role granted to the application login.
func WithTenantRoleSwitch(enabled bool) Option {
	return func(s *Store) {
		s.roleSwitch = enabled
	}
}

// SchemaFromSlug derives the PostgreSQL schema name from a tenant slug following
// the Azra platform convention ("tenant_" prefix, hyphens replaced with underscores).
// Example: "mayo-clinic" -> "tenant_mayo_clinic".
func SchemaFromSlug(slug string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(slug))
	if s == "" {
		return "", ErrNoTenant
	}
	if len(s) > maxTenantSlugLen || !validTenantSlug.MatchString(s) {
		return "", fmt.Errorf("%w: %q", ErrInvalidTenantSlug, slug)
	}
	return tenantSchemaPrefix + strings.ReplaceAll(s, "-", "_"), nil
}

// QuoteIdent quotes a SQL identifier to prevent injection, wrapping in double quotes
// and escaping embedded double quotes.
func QuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// ProvisionTenantSchema idempotently creates the "tenant_<slug>" schema and applies
// all embedded operational store migrations within that schema.
func ProvisionTenantSchema(ctx context.Context, db *sql.DB, tenantID string) (string, error) {
	schema, err := SchemaFromSlug(tenantID)
	if err != nil {
		return "", err
	}

	entries, err := fs.ReadDir(MigrationsFS, "migrations")
	if err != nil {
		return "", fmt.Errorf("read migrations dir: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin tenant schema migration tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	lockKey := "flint_tenant_schema:" + schema
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", lockKey); err != nil {
		return "", fmt.Errorf("acquire tenant schema advisory lock: %w", err)
	}

	quoted := QuoteIdent(schema)
	if _, err := tx.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+quoted); err != nil {
		return "", fmt.Errorf("create schema %s: %w", schema, err)
	}
	if _, err := tx.ExecContext(ctx, "SET LOCAL search_path TO "+quoted); err != nil {
		return "", fmt.Errorf("set search_path to %s: %w", schema, err)
	}

	for _, name := range names {
		sqlBytes, err := MigrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return "", fmt.Errorf("read migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(sqlBytes)); err != nil {
			return "", fmt.Errorf("apply migration %s in schema %s: %w", name, schema, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit tenant schema %s migrations: %w", schema, err)
	}
	return schema, nil
}

// EnsureTenantSchema resolves the PostgreSQL schema for tenantID ("tenant_<slug>")
// and ensures it is provisioned if auto-provisioning is enabled on the Store.
func (s *Store) EnsureTenantSchema(ctx context.Context, tenantID string) (string, error) {
	schema, err := SchemaFromSlug(tenantID)
	if err != nil {
		return "", err
	}
	if !s.autoProvision {
		return schema, nil
	}
	if _, ok := s.provisionedSchemas.Load(schema); ok {
		return schema, nil
	}
	if _, err := ProvisionTenantSchema(ctx, s.db, tenantID); err != nil {
		return "", err
	}
	s.provisionedSchemas.Store(schema, true)
	return schema, nil
}

// withTenantTx executes fn inside a database transaction isolated to the tenant's
// dedicated PostgreSQL schema ("tenant_<slug>") via SET LOCAL search_path (and
// optional SET LOCAL ROLE).
func (s *Store) withTenantTx(ctx context.Context, tenantID string, fn func(tx *sql.Tx) error) error {
	schema, err := s.EnsureTenantSchema(ctx, tenantID)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tenant tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	quoted := QuoteIdent(schema)
	isolateSQL := "SET LOCAL search_path TO " + quoted
	if s.roleSwitch {
		isolateSQL += "; SET LOCAL ROLE " + quoted
	}
	if _, err := tx.ExecContext(ctx, isolateSQL); err != nil {
		return fmt.Errorf("schema isolation (%s): %w", schema, err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tenant tx: %w", err)
	}
	return nil
}

// PurgeTenant deletes all FHIR resources, history, search indexes, audit events,
// and export jobs from the tenant's dedicated schema ("tenant_<slug>").
func PurgeTenant(ctx context.Context, db *sql.DB, tenantID string) error {
	schema, err := SchemaFromSlug(tenantID)
	if err != nil {
		return err
	}

	var exists bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.schemata WHERE schema_name = $1
		)
	`, schema).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	quoted := QuoteIdent(schema)
	if _, err := tx.ExecContext(ctx, "SET LOCAL search_path TO "+quoted); err != nil {
		return err
	}

	tables := []string{
		"fhir_export_job",
		"spidx_string",
		"spidx_token",
		"spidx_date",
		"spidx_reference",
		"spidx_quantity",
		"spidx_uri",
		"fhir_audit_event",
		"fhir_resource_history",
		"fhir_resource",
	}
	for _, table := range tables {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return fmt.Errorf("purge %s.%s: %w", schema, table, err)
		}
	}

	return tx.Commit()
}
