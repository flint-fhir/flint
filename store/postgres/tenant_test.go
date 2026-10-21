package postgres_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/flint-fhir/flint/store/postgres"
)

func TestSchemaFromSlug(t *testing.T) {
	tests := []struct {
		name    string
		slug    string
		want    string
		wantErr error
	}{
		{
			name: "hyphenated slug",
			slug: "mayo-clinic",
			want: "tenant_mayo_clinic",
		},
		{
			name: "single word slug",
			slug: "acme",
			want: "tenant_acme",
		},
		{
			name: "alphanumeric and underscores",
			slug: "tenant_01-dev",
			want: "tenant_tenant_01_dev",
		},
		{
			name:    "empty slug",
			slug:    "",
			wantErr: postgres.ErrNoTenant,
		},
		{
			name:    "whitespace only slug",
			slug:    "   ",
			wantErr: postgres.ErrNoTenant,
		},
		{
			name:    "leading hyphen rejected",
			slug:    "-bad-tenant",
			wantErr: postgres.ErrInvalidTenantSlug,
		},
		{
			name:    "trailing hyphen rejected",
			slug:    "bad-tenant-",
			wantErr: postgres.ErrInvalidTenantSlug,
		},
		{
			name:    "sql injection attempt rejected",
			slug:    `foo"; DROP SCHEMA public CASCADE; --`,
			wantErr: postgres.ErrInvalidTenantSlug,
		},
		{
			name:    "exceeds 63 byte postgres identifier limit",
			slug:    strings.Repeat("a", 57),
			wantErr: postgres.ErrInvalidTenantSlug,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := postgres.SchemaFromSlug(tc.slug)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("SchemaFromSlug(%q) error = %v, want %v", tc.slug, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("SchemaFromSlug(%q) unexpected error: %v", tc.slug, err)
			}
			if got != tc.want {
				t.Errorf("SchemaFromSlug(%q) = %q, want %q", tc.slug, got, tc.want)
			}
		})
	}
}

func TestQuoteIdent(t *testing.T) {
	if got := postgres.QuoteIdent("tenant_mayo_clinic"); got != `"tenant_mayo_clinic"` {
		t.Errorf("QuoteIdent simple = %q", got)
	}
	if got := postgres.QuoteIdent(`tenant_"evil"`); got != `"tenant_""evil"""` {
		t.Errorf("QuoteIdent escaped = %q", got)
	}
}

func TestSchemaPerTenant_PhysicalIsolation(t *testing.T) {
	dsn := os.Getenv("FLINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		dsn = "postgres://flint:flint@localhost:5432/flint?sslmode=disable"
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("skipping Postgres integration test (open failed): %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("skipping Postgres integration test (ping failed): %v", err)
	}

	ctx = context.Background()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	store := postgres.New(db)
	defer func() {
		_ = postgres.PurgeTenant(ctx, db, "alpha-clinic")
		_ = postgres.PurgeTenant(ctx, db, "beta-clinic")
	}()

	alphaProto := []byte{0x0a, 0x05, 'a', 'l', 'p', 'h', 'a'}
	betaProto := []byte{0x0a, 0x04, 'b', 'e', 't', 'a'}

	if err := store.WriteResource(ctx, postgres.ResourceInput{
		TenantID:      "alpha-clinic",
		ResType:       "Patient",
		ResID:         "pat-shared-id",
		ResourceProto: alphaProto,
	}); err != nil {
		t.Fatalf("WriteResource alpha-clinic: %v", err)
	}

	if err := store.WriteResource(ctx, postgres.ResourceInput{
		TenantID:      "beta-clinic",
		ResType:       "Patient",
		ResID:         "pat-shared-id",
		ResourceProto: betaProto,
	}); err != nil {
		t.Fatalf("WriteResource beta-clinic: %v", err)
	}

	// Verify dedicated schemas exist physically in PostgreSQL
	for _, wantSchema := range []string{"tenant_alpha_clinic", "tenant_beta_clinic"} {
		var exists bool
		if err := db.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)",
			wantSchema,
		).Scan(&exists); err != nil {
			t.Fatalf("check schema %s: %v", wantSchema, err)
		}
		if !exists {
			t.Errorf("expected physical schema %q to exist", wantSchema)
		}
	}

	// Verify physical rows live in their respective tenant schemas and NOT in public
	var alphaBytes, betaBytes []byte
	if err := db.QueryRowContext(ctx,
		"SELECT resource_proto FROM tenant_alpha_clinic.fhir_resource WHERE res_type = 'Patient' AND res_id = 'pat-shared-id'",
	).Scan(&alphaBytes); err != nil {
		t.Fatalf("query tenant_alpha_clinic.fhir_resource: %v", err)
	}
	if !bytes.Equal(alphaBytes, alphaProto) {
		t.Errorf("tenant_alpha_clinic proto = %x, want %x", alphaBytes, alphaProto)
	}

	if err := db.QueryRowContext(ctx,
		"SELECT resource_proto FROM tenant_beta_clinic.fhir_resource WHERE res_type = 'Patient' AND res_id = 'pat-shared-id'",
	).Scan(&betaBytes); err != nil {
		t.Fatalf("query tenant_beta_clinic.fhir_resource: %v", err)
	}
	if !bytes.Equal(betaBytes, betaProto) {
		t.Errorf("tenant_beta_clinic proto = %x, want %x", betaBytes, betaProto)
	}

	var publicCount int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM public.fhir_resource WHERE res_id = 'pat-shared-id'",
	).Scan(&publicCount); err != nil {
		t.Fatalf("query public.fhir_resource: %v", err)
	}
	if publicCount != 0 {
		t.Errorf("expected 0 rows in public.fhir_resource, got %d", publicCount)
	}

	// Verify cross-tenant batch is rejected
	err = store.WriteBatch(ctx, []postgres.ResourceInput{
		{TenantID: "alpha-clinic", ResType: "Patient", ResID: "p1", ResourceProto: alphaProto},
		{TenantID: "beta-clinic", ResType: "Patient", ResID: "p2", ResourceProto: betaProto},
	})
	if !errors.Is(err, postgres.ErrCrossTenantBatch) {
		t.Errorf("WriteBatch cross-tenant error = %v, want ErrCrossTenantBatch", err)
	}
}
