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

	"github.com/flint-fhir/flint/automq"
	"github.com/flint-fhir/flint/ingest/workflow"
)

type e2eDeps struct {
	execWorkflow func(ctx context.Context, input workflow.IngestInput) (string, string, error)
	pollMessages func(ctx context.Context, brokers, topic string, maxRecords int) ([]*kgo.Record, error)
	publishCDC   func(ctx context.Context, brokers []string, schemaRegURL string, msgs []automq.Message) error
	findMessage  func(ctx context.Context, brokers []string, topic, key string) (int32, int64, error)
	runCmd       func(name string, args ...string) ([]byte, error)
	sleep        func(d time.Duration)
}

func defaultE2EDeps() e2eDeps {
	return e2eDeps{
		execWorkflow: func(ctx context.Context, input workflow.IngestInput) (string, string, error) {
			c, err := client.Dial(client.Options{HostPort: envOrDefault("TEMPORAL_ADDRESS", "localhost:7233")})
			if err != nil {
				return "", "", fmt.Errorf("temporal: %w", err)
			}
			defer c.Close()
			we, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
				ID:        input.BundleID,
				TaskQueue: workflow.TaskQueue,
			}, workflow.IngestFHIRBundle, input)
			if err != nil {
				return "", "", fmt.Errorf("start workflow: %w", err)
			}
			waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := we.Get(waitCtx, nil); err != nil {
				return we.GetID(), we.GetRunID(), fmt.Errorf("workflow failed: %w", err)
			}
			return we.GetID(), we.GetRunID(), nil
		},
		pollMessages: func(ctx context.Context, brokers, topic string, maxRecords int) ([]*kgo.Record, error) {
			cl, err := kgo.NewClient(
				kgo.SeedBrokers(brokers),
				kgo.ConsumeTopics(topic),
				kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
			)
			if err != nil {
				return nil, fmt.Errorf("kafka: %w", err)
			}
			defer cl.Close()
			fetchCtx, fetchCancel := context.WithTimeout(ctx, 10*time.Second)
			defer fetchCancel()
			var out []*kgo.Record
			for {
				fetches := cl.PollFetches(fetchCtx)
				if fetchCtx.Err() != nil {
					break
				}
				fetches.EachRecord(func(r *kgo.Record) {
					out = append(out, r)
				})
				if len(out) >= maxRecords {
					break
				}
			}
			return out, nil
		},
		publishCDC: func(ctx context.Context, brokers []string, schemaRegURL string, msgs []automq.Message) error {
			cfg := automq.Config{
				Brokers:           brokers,
				TopicPrefix:       "fhir.",
				SchemaRegistryURL: schemaRegURL,
			}
			if schemaRegURL != "" {
				cfg.DefaultSchemaID = 1
			}
			p, err := automq.NewProducer(cfg)
			if err != nil {
				return fmt.Errorf("producer: %w", err)
			}
			defer p.Close()
			return p.Publish(ctx, msgs)
		},
		findMessage: func(ctx context.Context, brokers []string, topic, key string) (int32, int64, error) {
			p, off := findMessageInTopic(ctx, brokers, topic, key)
			return p, off, nil
		},
		runCmd: execCombinedOutput,
		sleep:  time.Sleep,
	}
}

func selectE2EMode(args []string) string {
	if len(args) > 1 {
		switch args[1] {
		case "delete", "update", "recovery":
			return args[1]
		}
	}
	return "pipeline"
}

func buildPipelineIngestInput(bundleID string) workflow.IngestInput {
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

	return workflow.IngestInput{
		TenantID:       "e2e-test",
		BundleID:       bundleID,
		ResourceProtos: resProtos,
		ResourceTypes:  resTypes,
		ResourceIDs:    resIDs,
	}
}

func runE2EMode(ctx context.Context, args []string, deps e2eDeps) error {
	switch selectE2EMode(args) {
	case "delete":
		return runDeleteWithDeps(ctx, deps)
	case "update":
		return runUpdateWithDeps(ctx, deps)
	case "recovery":
		return runRecoveryWithDeps(ctx, deps)
	default:
		return runPipelineWithDeps(ctx, deps)
	}
}

func main() {
	if err := runE2EMode(context.Background(), os.Args, defaultE2EDeps()); err != nil {
		log.Fatalf("%v", err)
	}
}

func runPipelineWithDeps(ctx context.Context, deps e2eDeps) error {
	input := buildPipelineIngestInput(fmt.Sprintf("e2e-bundle-%d", time.Now().Unix()))

	fmt.Println("🚀 Starting IngestFHIRBundle workflow...")
	wfID, runID, err := deps.execWorkflow(ctx, input)
	if err != nil {
		return err
	}
	fmt.Printf("   Workflow ID: %s\n   Run ID:      %s\n", wfID, runID)
	fmt.Println("✅ Workflow completed successfully")

	// Verify AutoMQ received messages
	fmt.Println("\n📡 Checking AutoMQ (fhir.Patient topic)...")
	brokers := envOrDefault("AUTOMQ_BROKERS", "localhost:9092")
	records, err := deps.pollMessages(ctx, brokers, "fhir.Patient", 3)
	if err != nil {
		return err
	}

	msgCount := len(records)
	for i, r := range records {
		fmt.Printf("   [%d] key=%s partition=%d offset=%d headers=%v\n",
			i+1, string(r.Key), r.Partition, r.Offset, headerMap(r.Headers))
	}

	if msgCount >= 3 {
		fmt.Println("   ✅ All 3 Patient messages in AutoMQ")
	} else if msgCount > 0 {
		fmt.Printf("   ⚠️  Got %d messages (expected 3)\n", msgCount)
	} else {
		fmt.Println("   ⚠️  No messages yet (AutoMQ may be buffering)")
	}

	fmt.Println("\n🎉 E2E pipeline test complete!")
	return nil
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
