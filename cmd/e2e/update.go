package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/flint-fhir/flint/ingest/workflow"
)

func buildUpdateIngestInput(bundleID string) workflow.IngestInput {
	blob, _ := json.Marshal(map[string]any{
		"resourceType": "Patient",
		"id":           map[string]string{"value": "e2e-wf-001"},
		"name": []map[string]any{{
			"family": map[string]string{"value": "Wonderland"},
			"given":  []map[string]string{{"value": "Alice"}},
		}},
		"gender": map[string]string{"value": "female"},
	})

	return workflow.IngestInput{
		TenantID:       "e2e-test",
		BundleID:       bundleID,
		ResourceProtos: [][]byte{blob},
		ResourceTypes:  []string{"Patient"},
		ResourceIDs:    []string{"e2e-wf-001"},
	}
}

func runUpdateWithDeps(ctx context.Context, deps e2eDeps) error {
	input := buildUpdateIngestInput(fmt.Sprintf("e2e-update-%d", time.Now().Unix()))

	fmt.Println("🚀 Sending update for Patient e2e-wf-001 (Family -> Wonderland)...")
	if _, _, err := deps.execWorkflow(ctx, input); err != nil {
		return err
	}
	fmt.Println("✅ Update workflow completed successfully!")
	return nil
}
