package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/flint-fhir/flint/automq"
	"github.com/flint-fhir/flint/ingest/workflow"
)

func TestSelectE2EModeAndHelpers(t *testing.T) {
	assert.Equal(t, "pipeline", selectE2EMode([]string{"e2e"}))
	assert.Equal(t, "pipeline", selectE2EMode([]string{"e2e", "unknown"}))
	assert.Equal(t, "delete", selectE2EMode([]string{"e2e", "delete"}))
	assert.Equal(t, "update", selectE2EMode([]string{"e2e", "update"}))
	assert.Equal(t, "recovery", selectE2EMode([]string{"e2e", "recovery"}))

	t.Setenv("FLINT_TEST_E2E_ENV", "custom-val")
	assert.Equal(t, "custom-val", envOrDefault("FLINT_TEST_E2E_ENV", "def"))
	assert.Equal(t, "def", envOrDefault("FLINT_TEST_E2E_MISSING", "def"))

	hm := headerMap([]kgo.RecordHeader{
		{Key: "tenant_id", Value: []byte("e2e-test")},
		{Key: "op", Value: []byte("C")},
	})
	assert.Equal(t, map[string]string{"tenant_id": "e2e-test", "op": "C"}, hm)
}

func TestE2EBuilders(t *testing.T) {
	pipeIn := buildPipelineIngestInput("b-1")
	assert.Equal(t, "e2e-test", pipeIn.TenantID)
	assert.Equal(t, "b-1", pipeIn.BundleID)
	assert.Equal(t, []string{"e2e-wf-001", "e2e-wf-002", "e2e-wf-003"}, pipeIn.ResourceIDs)
	assert.Equal(t, []string{"Patient", "Patient", "Patient"}, pipeIn.ResourceTypes)
	require.Len(t, pipeIn.ResourceProtos, 3)

	delMsg := buildDeleteCDCMessage()
	assert.Equal(t, "e2e-test", delMsg.TenantID)
	assert.Equal(t, "Patient", delMsg.ResType)
	assert.Equal(t, "e2e-wf-003", delMsg.ResID)
	assert.Equal(t, "D", delMsg.Op)
	assert.Empty(t, delMsg.ProtoData)

	updIn := buildUpdateIngestInput("b-upd-1")
	assert.Equal(t, "e2e-test", updIn.TenantID)
	assert.Equal(t, []string{"e2e-wf-001"}, updIn.ResourceIDs)
	require.Len(t, updIn.ResourceProtos, 1)
	assert.Contains(t, string(updIn.ResourceProtos[0]), "Wonderland")

	recMsg := buildRecoveryMessage("e2e-rec-1", "PreRestart", "Arthur")
	assert.Equal(t, "e2e-test", recMsg.TenantID)
	assert.Equal(t, "Patient", recMsg.ResType)
	assert.Equal(t, "e2e-rec-1", recMsg.ResID)
	assert.Equal(t, "C", recMsg.Op)
	var recPayload map[string]any
	require.NoError(t, json.Unmarshal(recMsg.ProtoData, &recPayload))
	assert.Equal(t, "Patient", recPayload["resourceType"])
}

func TestRunE2EMode_AllModesAndErrorBranches(t *testing.T) {
	ctx := t.Context()

	baseDeps := e2eDeps{
		execWorkflow: func(_ context.Context, input workflow.IngestInput) (string, string, error) {
			return input.BundleID, "run-123", nil
		},
		pollMessages: func(_ context.Context, _, _ string, _ int) ([]*kgo.Record, error) {
			return []*kgo.Record{
				{Key: []byte("k1"), Partition: 0, Offset: 1, Headers: []kgo.RecordHeader{{Key: "op", Value: []byte("C")}}},
				{Key: []byte("k2"), Partition: 0, Offset: 2},
				{Key: []byte("k3"), Partition: 0, Offset: 3},
			}, nil
		},
		publishCDC: func(_ context.Context, _ []string, _ string, _ []automq.Message) error {
			return nil
		},
		findMessage: func(_ context.Context, _ []string, _, _ string) (int32, int64, error) {
			return 0, 42, nil
		},
		runCmd: func(_ string, _ ...string) ([]byte, error) {
			return []byte("ok"), nil
		},
		sleep: func(_ time.Duration) {},
	}

	// 1. Pipeline mode (3 messages, 1 message, 0 messages, workflow error, poll error)
	require.NoError(t, runE2EMode(ctx, []string{"e2e"}, baseDeps))

	deps1Msg := baseDeps
	deps1Msg.pollMessages = func(_ context.Context, _, _ string, _ int) ([]*kgo.Record, error) {
		return []*kgo.Record{{Key: []byte("k1")}}, nil
	}
	require.NoError(t, runPipelineWithDeps(ctx, deps1Msg))

	deps0Msg := baseDeps
	deps0Msg.pollMessages = func(_ context.Context, _, _ string, _ int) ([]*kgo.Record, error) {
		return nil, nil
	}
	require.NoError(t, runPipelineWithDeps(ctx, deps0Msg))

	depsWfErr := baseDeps
	depsWfErr.execWorkflow = func(_ context.Context, _ workflow.IngestInput) (string, string, error) {
		return "", "", errors.New("wf fail")
	}
	require.ErrorContains(t, runPipelineWithDeps(ctx, depsWfErr), "wf fail")

	depsPollErr := baseDeps
	depsPollErr.pollMessages = func(_ context.Context, _, _ string, _ int) ([]*kgo.Record, error) {
		return nil, errors.New("poll fail")
	}
	require.ErrorContains(t, runPipelineWithDeps(ctx, depsPollErr), "poll fail")

	// 2. Delete mode (success + publish error)
	require.NoError(t, runE2EMode(ctx, []string{"e2e", "delete"}, baseDeps))
	depsPubErr := baseDeps
	depsPubErr.publishCDC = func(_ context.Context, _ []string, _ string, _ []automq.Message) error {
		return errors.New("pub fail")
	}
	require.ErrorContains(t, runDeleteWithDeps(ctx, depsPubErr), "pub fail")

	// 3. Update mode (success + workflow error)
	require.NoError(t, runE2EMode(ctx, []string{"e2e", "update"}, baseDeps))
	require.ErrorContains(t, runUpdateWithDeps(ctx, depsWfErr), "wf fail")

	// 4. Recovery mode (success + pre-publish err + find err + kubectl restart err + kubectl status err + post-publish err + duckdb warning)
	require.NoError(t, runE2EMode(ctx, []string{"e2e", "recovery"}, baseDeps))
	require.ErrorContains(t, runRecoveryWithDeps(ctx, depsPubErr), "publish pre-restart")

	depsFindErr := baseDeps
	depsFindErr.findMessage = func(_ context.Context, _ []string, _, _ string) (int32, int64, error) {
		return 0, 0, errors.New("find fail")
	}
	require.ErrorContains(t, runRecoveryWithDeps(ctx, depsFindErr), "find fail")

	depsRestartErr := baseDeps
	depsRestartErr.runCmd = func(_ string, args ...string) ([]byte, error) {
		if len(args) > 1 && args[1] == "restart" {
			return []byte("err"), errors.New("restart fail")
		}
		return []byte("ok"), nil
	}
	require.ErrorContains(t, runRecoveryWithDeps(ctx, depsRestartErr), "kubectl rollout restart failed")

	depsStatusErr := baseDeps
	depsStatusErr.runCmd = func(_ string, args ...string) ([]byte, error) {
		if len(args) > 1 && args[1] == "status" {
			return []byte("err"), errors.New("status fail")
		}
		return []byte("ok"), nil
	}
	require.ErrorContains(t, runRecoveryWithDeps(ctx, depsStatusErr), "kubectl rollout status failed")

	var pubCalls int
	depsPostPubErr := baseDeps
	depsPostPubErr.publishCDC = func(_ context.Context, _ []string, _ string, _ []automq.Message) error {
		pubCalls++
		if pubCalls == 2 {
			return errors.New("post pub fail")
		}
		return nil
	}
	require.ErrorContains(t, runRecoveryWithDeps(ctx, depsPostPubErr), "publish post-restart")

	var findCalls int
	depsPostFindErr := baseDeps
	depsPostFindErr.findMessage = func(_ context.Context, _ []string, _, _ string) (int32, int64, error) {
		findCalls++
		if findCalls == 2 {
			return 0, 0, errors.New("post find fail")
		}
		return 0, 1, nil
	}
	require.ErrorContains(t, runRecoveryWithDeps(ctx, depsPostFindErr), "post find fail")

	// verifyIcebergOutputWithRunner error branch + defaultE2EDeps closures & findMessageFromPoll
	verifyIcebergOutputWithRunner(func(_ string, _ ...string) ([]byte, error) {
		return []byte("duckdb err"), errors.New("duckdb fail")
	})
	d := defaultE2EDeps()
	assert.NotNil(t, d.execWorkflow)
	assert.NotNil(t, d.pollMessages)
	assert.NotNil(t, d.publishCDC)

	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	_, _ = d.pollMessages(canceledCtx, "127.0.0.1:19092", "fhir.Patient", 1)
	_ = d.publishCDC(canceledCtx, []string{"127.0.0.1:19092"}, "http://127.0.0.1:18081", nil)
	t.Setenv("TEMPORAL_ADDRESS", "invalid-host-port-format:::")
	_, _, _ = d.execWorkflow(canceledCtx, buildPipelineIngestInput("b-err"))

	// findMessageFromPoll: match found and timeout error
	p, off, err := findMessageFromPoll(ctx, "fhir.Patient", "target-key", func(_ context.Context) kgo.Fetches {
		return kgo.Fetches{{
			Topics: []kgo.FetchTopic{{
				Topic: "fhir.Patient",
				Partitions: []kgo.FetchPartition{{
					Partition: 2,
					Records: []*kgo.Record{
						{Key: []byte("other"), Partition: 2, Offset: 10},
						{Key: []byte("target-key"), Partition: 2, Offset: 11},
					},
				}},
			}},
		}}
	})
	require.NoError(t, err)
	assert.Equal(t, int32(2), p)
	assert.Equal(t, int64(11), off)

	_, _, err = findMessageFromPoll(canceledCtx, "fhir.Patient", "missing", func(_ context.Context) kgo.Fetches {
		return nil
	})
	require.ErrorContains(t, err, "timed out looking for key")
}
