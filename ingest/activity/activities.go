// Package activity defines the Temporal activities for FHIR Bundle ingestion.
package activity

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"sync"

	"github.com/flint-fhir/flint/automq"
	"github.com/flint-fhir/flint/store/postgres"
)

var (
	defaultExtractorsOnce sync.Once
	defaultExtractorsMap  map[string]IndexExtractorFunc
)

func getDefaultExtractors() map[string]IndexExtractorFunc {
	defaultExtractorsOnce.Do(func() {
		defaultExtractorsMap = postgres.DefaultIndexExtractors()
	})
	return defaultExtractorsMap
}

func isNilPublisher(p automq.Publisher) bool {
	if p == nil {
		return true
	}
	v := reflect.ValueOf(p)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

// --- Activity Inputs ---

// WriteMedplumBatchInput is the input for the WriteMedplumBatch activity.
type WriteMedplumBatchInput struct {
	TenantID       string
	ResourceProtos [][]byte
	ResourceTypes  []string
	ResourceIDs    []string
}

// WritePostgresBatchInput is the input for the WritePostgresBatch activity.
type WritePostgresBatchInput struct {
	TenantID       string
	BundleID       string
	ResourceProtos [][]byte
	ResourceTypes  []string
	ResourceIDs    []string
}

// PublishToAutoMQInput is the input for the PublishToAutoMQ activity.
type PublishToAutoMQInput struct {
	TenantID       string
	ResourceProtos [][]byte
	ResourceTypes  []string
	ResourceIDs    []string
	Op             string // "U" for upsert, "D" for delete
}

// --- Activities ---

// Activities holds dependencies for all ingest activities.
type Activities struct {
	Store    *postgres.Store
	Producer automq.Publisher // nil = skip AutoMQ publish
	Logger   *slog.Logger
	// IndexExtractors maps resource type → extraction function.
	// When nil or missing a resource type, WritePostgresBatch falls back to
	// postgres.DefaultIndexExtractors() covering all 146 FHIR R4 resource types.
	IndexExtractors map[string]IndexExtractorFunc
}

// IndexExtractorFunc extracts search indexes from a proto-encoded resource.
type IndexExtractorFunc = postgres.IndexExtractorFunc

func (a *Activities) logger() *slog.Logger {
	if a != nil && a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}

// WriteMedplumBatch forwards FHIR resources to Medplum.
// This is temporary — Medplum will be replaced by Flint's own FHIR server in H3.
func (a *Activities) WriteMedplumBatch(_ context.Context, input WriteMedplumBatchInput) error {
	a.logger().Info("WriteMedplumBatch (stub)",
		"tenant", input.TenantID,
		"resources", len(input.ResourceProtos),
	)
	return nil
}

// WritePostgresBatch writes resources to Postgres in a single transaction.
// This is the hot path — upsert resource blob + delete old indexes + bulk insert new indexes.
func (a *Activities) WritePostgresBatch(ctx context.Context, input WritePostgresBatchInput) error {
	if len(input.ResourceTypes) != len(input.ResourceProtos) || len(input.ResourceIDs) != len(input.ResourceProtos) {
		return fmt.Errorf("mismatched batch input lengths: protos=%d, types=%d, ids=%d",
			len(input.ResourceProtos), len(input.ResourceTypes), len(input.ResourceIDs))
	}
	if len(input.ResourceProtos) > 0 && a.Store == nil {
		return fmt.Errorf("postgres store is nil")
	}

	inputs := make([]postgres.ResourceInput, len(input.ResourceProtos))

	for i := range input.ResourceProtos {
		resType := input.ResourceTypes[i]
		resID := input.ResourceIDs[i]

		ri := postgres.ResourceInput{
			TenantID:       input.TenantID,
			ResType:        resType,
			ResID:          resID,
			ResourceProto:  input.ResourceProtos[i],
			IdempotencyKey: input.BundleID,
		}

		// Extract search indexes using configured extractor or universal 146-resource fallback
		var extractor IndexExtractorFunc
		if a.IndexExtractors != nil {
			extractor = a.IndexExtractors[resType]
		}
		if extractor == nil {
			extractor = getDefaultExtractors()[resType]
		}
		if extractor != nil {
			idx, err := extractor(input.TenantID, resID, input.ResourceProtos[i])
			if err != nil {
				a.logger().Warn("index extraction failed, storing without indexes",
					"type", resType, "id", resID, "error", err)
			} else {
				ri.SearchIndexes = idx
			}
		}

		inputs[i] = ri
	}

	if a.Store != nil {
		if err := a.Store.WriteBatch(ctx, inputs); err != nil {
			return fmt.Errorf("postgres batch write: %w", err)
		}
	}

	a.logger().Info("WritePostgresBatch complete",
		"tenant", input.TenantID,
		"resources", len(inputs),
		"bundle", input.BundleID,
	)
	return nil
}

// PublishToAutoMQ publishes per-resource-type proto messages to AutoMQ topics.
// Each resource type maps to a topic: {TopicPrefix}{ResourceType} (e.g., "fhir.Patient").
// Messages are keyed by resource ID for partition locality and log compaction.
func (a *Activities) PublishToAutoMQ(ctx context.Context, input PublishToAutoMQInput) error {
	if len(input.ResourceTypes) != len(input.ResourceProtos) || len(input.ResourceIDs) != len(input.ResourceProtos) {
		return fmt.Errorf("mismatched automq input lengths: protos=%d, types=%d, ids=%d",
			len(input.ResourceProtos), len(input.ResourceTypes), len(input.ResourceIDs))
	}
	if isNilPublisher(a.Producer) {
		a.logger().Info("PublishToAutoMQ skipped (no producer configured)",
			"tenant", input.TenantID,
			"resources", len(input.ResourceProtos),
		)
		return nil
	}

	messages := make([]automq.Message, len(input.ResourceProtos))
	for i := range input.ResourceProtos {
		messages[i] = automq.Message{
			TenantID:  input.TenantID,
			ResType:   input.ResourceTypes[i],
			ResID:     input.ResourceIDs[i],
			ProtoData: input.ResourceProtos[i],
			Op:        input.Op,
		}
	}

	if err := a.Producer.Publish(ctx, messages); err != nil {
		return fmt.Errorf("automq publish: %w", err)
	}

	a.logger().Info("PublishToAutoMQ complete",
		"tenant", input.TenantID,
		"resources", len(messages),
		"op", input.Op,
	)
	return nil
}
