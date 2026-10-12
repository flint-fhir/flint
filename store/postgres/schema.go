package postgres

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// MigrationsFS embeds all versioned plain-SQL migrations in lexical order.
//
//go:embed migrations/*.sql
var MigrationsFS embed.FS

// RunMigrations applies all embedded SQL migrations in lexical order against db.
// All migration statements are idempotent (CREATE TABLE/INDEX IF NOT EXISTS), so
// RunMigrations can be safely called at test setup or local bootstrap with zero
// external CLI dependencies.
func RunMigrations(ctx context.Context, db *sql.DB) error {
	entries, err := fs.ReadDir(MigrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
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
		return fmt.Errorf("begin migration tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext('flint_schema_migrations'))"); err != nil {
		return fmt.Errorf("acquire migration advisory lock: %w", err)
	}

	for _, name := range names {
		sqlBytes, err := MigrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(sqlBytes)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}
	return nil
}
