package activity_test

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"testing"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	dtpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/datatypes_go_proto"
	condpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/condition_go_proto"
	patpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto"

	"github.com/flint-fhir/flint/ingest/activity"
	"github.com/flint-fhir/flint/store/postgres"
)

func TestWritePostgresBatch_WithExtractors(t *testing.T) {
	dsn := os.Getenv("FLINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("FLINT_TEST_POSTGRES_DSN not set")
	}

	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()

	migration, err := os.ReadFile("../../store/postgres/migrations/001_core_schema.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), string(migration))
	require.NoError(t, err)

	store := postgres.New(db)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	activities := &activity.Activities{
		Store:           store,
		Logger:          logger,
		IndexExtractors: postgres.DefaultIndexExtractors(),
	}

	// Create test Patient and Condition protos
	pat := &patpb.Patient{
		Id:     &dtpb.Id{Value: "act-pat-1"},
		Active: &dtpb.Boolean{Value: true},
		Name: []*dtpb.HumanName{
			{Family: &dtpb.String{Value: "Curie"}, Given: []*dtpb.String{{Value: "Marie"}}},
		},
	}
	patBytes, err := proto.Marshal(pat)
	require.NoError(t, err)

	cond := &condpb.Condition{
		Id: &dtpb.Id{Value: "act-cond-1"},
		Code: &dtpb.CodeableConcept{
			Coding: []*dtpb.Coding{
				{
					System: &dtpb.Uri{Value: "http://snomed.info/sct"},
					Code:   &dtpb.Code{Value: "44054006"},
				},
			},
		},
	}
	condBytes, err := proto.Marshal(cond)
	require.NoError(t, err)

	input := activity.WritePostgresBatchInput{
		TenantID:       "test-act",
		BundleID:       "bnd-act-1",
		ResourceProtos: [][]byte{patBytes, condBytes},
		ResourceTypes:  []string{"Patient", "Condition"},
		ResourceIDs:    []string{"act-pat-1", "act-cond-1"},
	}

	ctx := context.Background()
	err = activities.WritePostgresBatch(ctx, input)
	require.NoError(t, err)

	// Verify Patient is searchable by family name
	patResults, patTotal, err := store.Search(ctx, postgres.SearchParams{
		TenantID: "test-act",
		ResType:  "Patient",
		Strings:  map[string]string{"family": "curie"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, patTotal)
	require.Len(t, patResults, 1)
	assert.Equal(t, "act-pat-1", patResults[0].ResID)

	// Verify Condition is searchable by code
	condResults, condTotal, err := store.Search(ctx, postgres.SearchParams{
		TenantID: "test-act",
		ResType:  "Condition",
		Tokens:   map[string]string{"code": "44054006"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, condTotal)
	require.Len(t, condResults, 1)
	assert.Equal(t, "act-cond-1", condResults[0].ResID)

	// Cleanup
	for _, table := range []string{"spidx_string", "spidx_token", "spidx_date", "spidx_reference", "spidx_quantity", "spidx_uri", "fhir_resource"} {
		db.ExecContext(ctx, "DELETE FROM "+table+" WHERE tenant_id = 'test-act'")
	}
}

func TestPublishToAutoMQ_NilProducer(t *testing.T) {
	activities := &activity.Activities{
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	err := activities.PublishToAutoMQ(context.Background(), activity.PublishToAutoMQInput{
		TenantID: "test-tenant",
	})
	assert.NoError(t, err)
}
