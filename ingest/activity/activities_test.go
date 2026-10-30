package activity_test

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	dtpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/datatypes_go_proto"
	condpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/condition_go_proto"
	patpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto"

	"github.com/flint-fhir/flint/automq"
	"github.com/flint-fhir/flint/ingest/activity"
	"github.com/flint-fhir/flint/store/postgres"
)

type failingPublisher struct{}

func (failingPublisher) Publish(_ context.Context, _ []automq.Message) error {
	return errors.New("simulated broker failure")
}

func TestWritePostgresBatch_WithExtractors(t *testing.T) {
	dsn := os.Getenv("FLINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		dsn = "postgres://flint:flint@localhost:5432/flint?sslmode=disable"
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("skipping Postgres activity test (open failed): %v", err)
	}
	defer db.Close()

	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		t.Skipf("skipping Postgres activity test (ping failed): %v", err)
	}

	err = postgres.RunMigrations(t.Context(), db)
	require.NoError(t, err)

	store := postgres.New(db)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	activities := &activity.Activities{
		Store:  store,
		Logger: logger,
		// Omit IndexExtractors to verify automatic fallback to DefaultIndexExtractors (all 146 R4 types)
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
	patRes, err := store.Search(ctx, postgres.SearchParams{
		TenantID: "test-act",
		ResType:  "Patient",
		Strings:  map[string]string{"family": "curie"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, patRes.Total)
	require.Len(t, patRes.Matches, 1)
	assert.Equal(t, "act-pat-1", patRes.Matches[0].ResID)

	// Verify Condition is searchable by code
	condRes, err := store.Search(ctx, postgres.SearchParams{
		TenantID: "test-act",
		ResType:  "Condition",
		Tokens:   map[string]string{"code": "44054006"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, condRes.Total)
	require.Len(t, condRes.Matches, 1)
	assert.Equal(t, "act-cond-1", condRes.Matches[0].ResID)

	// Verify custom IndexExtractors error fallback (warns and still writes resource)
	customActivities := &activity.Activities{
		Store: store,
		// Omit Logger to exercise default slog.Default() fallback
		IndexExtractors: map[string]activity.IndexExtractorFunc{
			"Patient": func(_, _ string, _ []byte) (*postgres.SearchIndexes, error) {
				return nil, errors.New("synthetic extractor failure")
			},
		},
	}
	err = customActivities.WritePostgresBatch(ctx, activity.WritePostgresBatchInput{
		TenantID:       "test-act",
		BundleID:       "bnd-act-2",
		ResourceProtos: [][]byte{patBytes},
		ResourceTypes:  []string{"Patient"},
		ResourceIDs:    []string{"act-pat-fallback"},
	})
	require.NoError(t, err)
	rawFallback, err := store.ReadResource(ctx, "test-act", "Patient", "act-pat-fallback")
	require.NoError(t, err)
	assert.NotEmpty(t, rawFallback)

	// Cleanup
	_ = postgres.PurgeTenant(ctx, db, "test-act")
}

func TestPublishToAutoMQ_NilAndMemoryPublisher(t *testing.T) {
	var typedNilProducer *automq.Producer
	activities := &activity.Activities{
		Producer: typedNilProducer,
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	err := activities.PublishToAutoMQ(context.Background(), activity.PublishToAutoMQInput{
		TenantID:       "test-tenant",
		ResourceProtos: [][]byte{{1, 2, 3}},
		ResourceTypes:  []string{"Patient"},
		ResourceIDs:    []string{"pat-1"},
		Op:             "U",
	})
	require.NoError(t, err)

	memPub := automq.NewMemoryPublisher()
	activities.Producer = memPub
	err = activities.PublishToAutoMQ(context.Background(), activity.PublishToAutoMQInput{
		TenantID:       "test-tenant",
		ResourceProtos: [][]byte{{1, 2, 3}},
		ResourceTypes:  []string{"Patient"},
		ResourceIDs:    []string{"pat-1"},
		Op:             "U",
	})
	require.NoError(t, err)
	msgs := memPub.Messages()
	require.Len(t, msgs, 1)
	assert.Equal(t, "test-tenant", msgs[0].TenantID)
	assert.Equal(t, "Patient", msgs[0].ResType)
	assert.Equal(t, "pat-1", msgs[0].ResID)
	assert.Equal(t, "U", msgs[0].Op)

	err = activities.WriteMedplumBatch(context.Background(), activity.WriteMedplumBatchInput{
		TenantID:       "test-tenant",
		ResourceProtos: [][]byte{{1, 2, 3}},
		ResourceTypes:  []string{"Patient"},
		ResourceIDs:    []string{"pat-1"},
	})
	require.NoError(t, err)

	activities.Producer = failingPublisher{}
	err = activities.PublishToAutoMQ(context.Background(), activity.PublishToAutoMQInput{
		TenantID:       "test-tenant",
		ResourceProtos: [][]byte{{1, 2, 3}},
		ResourceTypes:  []string{"Patient"},
		ResourceIDs:    []string{"pat-1"},
		Op:             "U",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "automq publish")
}

func TestActivities_SliceBoundsAndNilStoreValidation(t *testing.T) {
	activities := &activity.Activities{
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	err := activities.WritePostgresBatch(context.Background(), activity.WritePostgresBatchInput{
		TenantID:       "test-tenant",
		ResourceProtos: [][]byte{{1, 2}},
		ResourceTypes:  []string{},
		ResourceIDs:    []string{"id-1"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mismatched batch input lengths")

	err = activities.WritePostgresBatch(context.Background(), activity.WritePostgresBatchInput{
		TenantID:       "test-tenant",
		ResourceProtos: [][]byte{{1, 2}},
		ResourceTypes:  []string{"Patient"},
		ResourceIDs:    []string{"id-1"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "postgres store is nil")

	err = activities.PublishToAutoMQ(context.Background(), activity.PublishToAutoMQInput{
		TenantID:       "test-tenant",
		ResourceProtos: [][]byte{{1, 2}},
		ResourceTypes:  []string{"Patient"},
		ResourceIDs:    []string{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mismatched automq input lengths")
}
