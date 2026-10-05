// Package workflow defines the IngestFHIRBundle Temporal workflow.
//
// This is the core ingest pipeline for Flint. A single FHIR Bundle triggers
// one workflow instance. The workflow orchestrates three activities in sequence:
//
//  1. WriteMedplumBatch — forward resources to Medplum (temporary FHIR API for apps)
//  2. WritePostgresBatch — upsert resource blobs + search indexes to Postgres
//  3. PublishToAutoMQ — publish per-resource-type proto messages with op field
//
// Dedup: the Bundle ID (or client-supplied idempotency key) is used as the
// Temporal workflow ID, providing exactly-once semantics.
package workflow

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const TaskQueue = "flint-ingest"

// IngestInput is the input to the IngestFHIRBundle workflow.
type IngestInput struct {
	TenantID       string
	BundleID       string   // idempotency key — used as Temporal workflow ID
	ResourceProtos [][]byte // proto-encoded FHIR resources
	ResourceTypes  []string // parallel array: resource type per proto
	ResourceIDs    []string // parallel array: resource ID per proto
}

// Activity input types — serialized over the wire to activity workers.
// Field names must match the corresponding types in the activity package.

type writeMedplumBatchInput struct {
	TenantID       string
	ResourceProtos [][]byte
	ResourceTypes  []string
	ResourceIDs    []string
}

type writePostgresBatchInput struct {
	TenantID       string
	BundleID       string
	ResourceProtos [][]byte
	ResourceTypes  []string
	ResourceIDs    []string
}

type publishToAutoMQInput struct {
	TenantID       string
	ResourceProtos [][]byte
	ResourceTypes  []string
	ResourceIDs    []string
	Op             string
}

// IngestFHIRBundle is the Temporal workflow that processes a FHIR Bundle.
//
// Design decisions (from plan v7):
//   - R8: batch per FHIR Bundle, not per resource → 3x less Temporal overhead
//   - Idempotent: all activities use upserts, re-running is safe
//   - Sequential activities: Medplum → Postgres → AutoMQ
//     (Medplum can fail without blocking Postgres; we retry independently)
func IngestFHIRBundle(ctx workflow.Context, input IngestInput) error {
	// Activity options: 30s start-to-close, 3 retries with backoff
	activityOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2.0,
			MaximumAttempts:    3,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, activityOpts)

	// 1. Write to Medplum (temporary FHIR API for clinical apps)
	// This is a "best effort" — if Medplum is down, we log and continue.
	// The Postgres store is the source of truth.
	medplumInput := writeMedplumBatchInput{
		TenantID:       input.TenantID,
		ResourceProtos: input.ResourceProtos,
		ResourceTypes:  input.ResourceTypes,
		ResourceIDs:    input.ResourceIDs,
	}
	err := workflow.ExecuteActivity(ctx, "WriteMedplumBatch", medplumInput).Get(ctx, nil)
	if err != nil {
		// Log but don't fail — Medplum is temporary and not the source of truth
		workflow.GetLogger(ctx).Warn("medplum write failed, continuing", "error", err)
	}

	// 2. Write to Postgres (source of truth for FHIR server)
	// Single transaction: upsert blobs + delete old indexes + bulk insert new indexes
	postgresInput := writePostgresBatchInput{
		TenantID:       input.TenantID,
		BundleID:       input.BundleID,
		ResourceProtos: input.ResourceProtos,
		ResourceTypes:  input.ResourceTypes,
		ResourceIDs:    input.ResourceIDs,
	}
	err = workflow.ExecuteActivity(ctx, "WritePostgresBatch", postgresInput).Get(ctx, nil)
	if err != nil {
		return err // Postgres failure is fatal — retry the whole workflow
	}

	// 3. Publish to AutoMQ (for Iceberg via Table Topic)
	// Per-resource-type topics with op field for CDC
	publishInput := publishToAutoMQInput{
		TenantID:       input.TenantID,
		ResourceProtos: input.ResourceProtos,
		ResourceTypes:  input.ResourceTypes,
		ResourceIDs:    input.ResourceIDs,
		Op:             "U", // upsert (or "D" for delete)
	}
	err = workflow.ExecuteActivity(ctx, "PublishToAutoMQ", publishInput).Get(ctx, nil)
	if err != nil {
		return err // AutoMQ failure is fatal — data must reach Iceberg
	}

	return nil
}
