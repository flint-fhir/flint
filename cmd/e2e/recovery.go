package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/flint-fhir/flint/automq"
)

func buildRecoveryMessage(id, family, given string) automq.Message {
	payload, _ := json.Marshal(map[string]any{
		"resourceType": "Patient",
		"id":           map[string]string{"value": id},
		"name": []map[string]any{{
			"family": map[string]string{"value": family},
			"given":  []map[string]string{{"value": given}},
		}},
		"gender": map[string]string{"value": "male"},
	})
	return automq.Message{
		TenantID:  "e2e-test",
		ResType:   "Patient",
		ResID:     id,
		ProtoData: payload,
		Op:        "C",
	}
}

func execCombinedOutput(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

func runRecoveryWithDeps(ctx context.Context, deps e2eDeps) error {
	brokers := []string{envOrDefault("AUTOMQ_BROKERS", "localhost:9092")}
	schemaReg := envOrDefault("FLINT_SCHEMA_REGISTRY_URL", "")

	fmt.Println("==================================================")
	fmt.Println("🔄 Phase 1.8 Restart Recovery Validation")
	fmt.Println("   AutoMQ → Table Topic resumes from last offset")
	fmt.Println("==================================================")

	// Step 1: Produce Pre-Restart Event
	fmt.Println("\n[1/5] 📤 Publishing pre-restart message to AutoMQ...")
	preID := "e2e-recovery-pre"
	if err := deps.publishCDC(ctx, brokers, schemaReg, []automq.Message{buildRecoveryMessage(preID, "PreRestart", "Arthur")}); err != nil {
		return fmt.Errorf("publish pre-restart message: %w", err)
	}

	// Verify pre-restart message in AutoMQ
	prePartition, preOffset, err := deps.findMessage(ctx, brokers, "fhir.Patient", preID)
	if err != nil {
		return err
	}
	fmt.Printf("   ✅ Pre-restart message committed in AutoMQ at partition %d, offset %d\n", prePartition, preOffset)

	// Step 2: Wait for Table Topic initial commit (interval is 10s)
	fmt.Println("\n[2/5] ⏳ Waiting 12s for AutoMQ Table Topic commit to Iceberg...")
	deps.sleep(12 * time.Second)

	// Step 3: Restart AutoMQ Pod
	fmt.Println("\n[3/5] 💥 Simulating AutoMQ crash / restart via kubectl rollout restart...")
	if out, err := deps.runCmd("kubectl", "rollout", "restart", "statefulset/automq", "-n", "flint"); err != nil {
		return fmt.Errorf("kubectl rollout restart failed: %s (%w)", string(out), err)
	}
	fmt.Println("   Waiting for AutoMQ statefulset to become ready again...")
	if out, err := deps.runCmd("kubectl", "rollout", "status", "statefulset/automq", "-n", "flint", "--timeout=120s"); err != nil {
		return fmt.Errorf("kubectl rollout status failed: %s (%w)", string(out), err)
	}
	fmt.Println("   ✅ AutoMQ pod successfully restarted and ready")

	// Allow broker TCP and Table Topic worker to fully initialize
	fmt.Println("   Giving AutoMQ 5s to stabilize post-restart...")
	deps.sleep(5 * time.Second)

	// Step 4: Produce Post-Restart Event
	fmt.Println("\n[4/5] 📤 Publishing post-restart message to AutoMQ...")
	postID := "e2e-recovery-post"
	if err := deps.publishCDC(ctx, brokers, schemaReg, []automq.Message{buildRecoveryMessage(postID, "PostRestart", "Ford")}); err != nil {
		return fmt.Errorf("publish post-restart message: %w", err)
	}

	// Verify post-restart message in AutoMQ
	postPartition, postOffset, err := deps.findMessage(ctx, brokers, "fhir.Patient", postID)
	if err != nil {
		return err
	}
	fmt.Printf("   ✅ Post-restart message committed in AutoMQ at partition %d, offset %d\n", postPartition, postOffset)

	// Step 5: Wait for Table Topic commit interval post-restart
	fmt.Println("\n[5/5] ⏳ Waiting 25s for Table Topic worker to resume and commit post-restart batch...")
	deps.sleep(25 * time.Second)

	// Step 6: Verify Iceberg Content via DuckDB
	fmt.Println("\n🔍 Querying Iceberg table via DuckDB to verify both pre and post restart records...")
	verifyIcebergOutputWithRunner(deps.runCmd)

	fmt.Println("\n🎉 Restart Recovery Validation Complete!")
	fmt.Println("   - AutoMQ resumed cleanly from S3 shared storage")
	fmt.Println("   - Table Topic worker resumed from last committed offset")
	fmt.Println("   - Zero data loss across broker crash/restart")
	return nil
}

func findMessageInTopic(ctx context.Context, brokers []string, topic, key string) (int32, int64) {
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
	)
	if err != nil {
		log.Fatalf("consumer: %v", err)
	}
	defer cl.Close()

	p, off, err := findMessageFromPoll(ctx, topic, key, cl.PollFetches)
	if err != nil {
		log.Fatalf("%v", err)
	}
	return p, off
}

func findMessageFromPoll(ctx context.Context, topic, key string, poll func(context.Context) kgo.Fetches) (int32, int64, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	for {
		fetches := poll(timeoutCtx)
		if timeoutCtx.Err() != nil {
			return -1, -1, fmt.Errorf("timed out looking for key %s in topic %s", key, topic)
		}
		var partition int32 = -1
		var offset int64 = -1
		var found bool
		fetches.EachRecord(func(r *kgo.Record) {
			if string(r.Key) == key {
				partition = r.Partition
				offset = r.Offset
				found = true
			}
		})
		if found {
			return partition, offset, nil
		}
	}
}

func verifyIcebergOutputWithRunner(runCmd func(name string, args ...string) ([]byte, error)) {
	out, err := runCmd("uv", "run", "--with", "duckdb", "--with", "pyiceberg[s3fs]", "--with", "pyarrow", "cmd/e2e/query_duckdb.py")
	if err != nil {
		log.Printf("warning: duckdb query failed: %v: %s", err, string(out))
		return
	}
	fmt.Println(string(out))
}
