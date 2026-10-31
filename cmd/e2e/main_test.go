package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kgo"
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
