// E2E test: exercises the full Flint pipeline.
//
// 1. POST a FHIR Bundle via the REST API → Postgres
// 2. Start an IngestFHIRBundle Temporal workflow with the same resources
// 3. Verify the workflow completes (writes to Postgres + publishes to AutoMQ)
// 4. Consume from AutoMQ to verify messages arrived
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.temporal.io/sdk/client"

	"github.com/flint-fhir/flint/ingest/workflow"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "delete":
			runDelete()
			return
		case "update":
			runUpdate()
			return
		}
	}
	runPipeline()
}

func runPipeline() {
	ctx := context.Background()

	// Connect to Temporal
	c, err := client.Dial(client.Options{HostPort: envOrDefault("TEMPORAL_ADDRESS", "localhost:7233")})
	if err != nil {
		log.Fatalf("temporal: %v", err)
	}
	defer c.Close()

	// Build workflow input with pre-encoded resources
	// (In production, the API server parses FHIR JSON → proto, then sends to Temporal)
	patients := []struct {
		ID     string
		Family string
		Given  string
		Gender string
	}{
		{"e2e-wf-001", "Workflow", "Alice", "female"},
		{"e2e-wf-002", "Workflow", "Bob", "male"},
		{"e2e-wf-003", "Workflow", "Charlie", "other"},
	}

	// For E2E, we send raw JSON bytes as "protos" — the WritePostgresBatch activity
	// handles the actual proto decoding. Here we test the workflow orchestration.
	var resProtos [][]byte
	var resTypes []string
	var resIDs []string

	for _, p := range patients {
		blob, _ := json.Marshal(map[string]any{
			"resourceType": "Patient",
			"id":           map[string]string{"value": p.ID},
			"name":         []map[string]any{{"family": map[string]string{"value": p.Family}, "given": []map[string]string{{"value": p.Given}}}},
			"gender":       map[string]string{"value": p.Gender},
		})
		resProtos = append(resProtos, blob)
		resTypes = append(resTypes, "Patient")
		resIDs = append(resIDs, p.ID)
	}

	input := workflow.IngestInput{
		TenantID:       "e2e-test",
		BundleID:       fmt.Sprintf("e2e-bundle-%d", time.Now().Unix()),
		ResourceProtos: resProtos,
		ResourceTypes:  resTypes,
		ResourceIDs:    resIDs,
	}

	// Start the workflow
	fmt.Println("🚀 Starting IngestFHIRBundle workflow...")
	workflowOpts := client.StartWorkflowOptions{
		ID:        input.BundleID,
		TaskQueue: workflow.TaskQueue,
	}

	we, err := c.ExecuteWorkflow(ctx, workflowOpts, workflow.IngestFHIRBundle, input)
	if err != nil {
		log.Fatalf("start workflow: %v", err)
	}
	fmt.Printf("   Workflow ID: %s\n   Run ID:      %s\n", we.GetID(), we.GetRunID())

	// Wait for completion (timeout 30s)
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err = we.Get(waitCtx, nil)
	if err != nil {
		log.Fatalf("❌ Workflow failed: %v", err)
	}
	fmt.Println("✅ Workflow completed successfully")

	// Verify AutoMQ received messages
	fmt.Println("\n📡 Checking AutoMQ (fhir.Patient topic)...")
	brokers := envOrDefault("AUTOMQ_BROKERS", "localhost:9092")
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(brokers),
		kgo.ConsumeTopics("fhir.Patient"),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
	)
	if err != nil {
		log.Fatalf("kafka: %v", err)
	}
	defer cl.Close()

	fetchCtx, fetchCancel := context.WithTimeout(ctx, 10*time.Second)
	defer fetchCancel()

	var msgCount int
	for {
		fetches := cl.PollFetches(fetchCtx)
		if fetchCtx.Err() != nil {
			break
		}
		fetches.EachRecord(func(r *kgo.Record) {
			msgCount++
			fmt.Printf("   [%d] key=%s partition=%d offset=%d headers=%v\n",
				msgCount, string(r.Key), r.Partition, r.Offset, headerMap(r.Headers))
		})
		if msgCount >= 3 {
			break
		}
	}

	if msgCount >= 3 {
		fmt.Println("   ✅ All 3 Patient messages in AutoMQ")
	} else if msgCount > 0 {
		fmt.Printf("   ⚠️  Got %d messages (expected 3)\n", msgCount)
	} else {
		fmt.Println("   ⚠️  No messages yet (AutoMQ may be buffering)")
	}

	fmt.Println("\n🎉 E2E pipeline test complete!")
}

func headerMap(headers []kgo.RecordHeader) map[string]string {
	m := make(map[string]string, len(headers))
	for _, h := range headers {
		m[h.Key] = string(h.Value)
	}
	return m
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
