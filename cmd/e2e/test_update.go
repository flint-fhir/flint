package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/flint-fhir/flint/ingest/workflow"
)

func main() {
	ctx := context.Background()

	c, err := client.Dial(client.Options{HostPort: envOrDefault("TEMPORAL_ADDRESS", "localhost:7233")})
	if err != nil {
		log.Fatalf("temporal: %v", err)
	}
	defer c.Close()

	// Update Alice (e2e-wf-001) with family name "Wonderland"
	blob, _ := json.Marshal(map[string]any{
		"resourceType": "Patient",
		"id":           map[string]string{"value": "e2e-wf-001"},
		"name": []map[string]any{{
			"family": map[string]string{"value": "Wonderland"},
			"given":  []map[string]string{{"value": "Alice"}},
		}},
		"gender": map[string]string{"value": "female"},
	})

	input := workflow.IngestInput{
		TenantID:       "e2e-test",
		BundleID:       fmt.Sprintf("e2e-update-%d", time.Now().Unix()),
		ResourceProtos: [][]byte{blob},
		ResourceTypes:  []string{"Patient"},
		ResourceIDs:    []string{"e2e-wf-001"},
	}

	fmt.Println("🚀 Sending update for Patient e2e-wf-001 (Family -> Wonderland)...")
	we, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        input.BundleID,
		TaskQueue: workflow.TaskQueue,
	}, workflow.IngestFHIRBundle, input)
	if err != nil {
		log.Fatalf("execute: %v", err)
	}

	if err := we.Get(ctx, nil); err != nil {
		log.Fatalf("workflow failed: %v", err)
	}
	fmt.Println("✅ Update workflow completed successfully!")
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
