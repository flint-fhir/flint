package cdc_test

import (
	"context"
	"sync"
	"testing"

	"github.com/flint-fhir/flint/pkg/cdc"
)

func TestMemoryPublisher_PublishResetAndDefensiveCopy(t *testing.T) {
	pub := cdc.NewMemoryPublisher()
	if got := len(pub.Messages()); got != 0 {
		t.Fatalf("expected 0 initial messages, got %d", got)
	}

	raw := []byte{0x01, 0x02, 0x03}
	err := pub.Publish(context.Background(), []cdc.Message{
		{
			TenantID:  "tenant-a",
			ResType:   "Patient",
			ResID:     "pat-1",
			ProtoData: raw,
			Op:        "U",
		},
		{
			TenantID: "tenant-a",
			ResType:  "Observation",
			ResID:    "obs-1",
			Op:       "D",
		},
	})
	if err != nil {
		t.Fatalf("Publish failed: %v", err)
	}

	// Mutate caller's byte slice to verify defensive copy inside Publish.
	raw[0] = 0xFF
	msgs := pub.Messages()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].ProtoData[0] != 0x01 {
		t.Fatalf("expected defensive copy of ProtoData (0x01), got 0x%x", msgs[0].ProtoData[0])
	}

	// Mutate returned slice to verify defensive copy in Messages().
	msgs[0].ResID = "mutated"
	if pub.Messages()[0].ResID != "pat-1" {
		t.Fatalf("expected Messages() to return a defensive slice copy")
	}

	topics := cdc.TopicSet(pub.Messages())
	if len(topics) != 2 || topics[0] != "Patient" || topics[1] != "Observation" {
		t.Fatalf("unexpected TopicSet: %v", topics)
	}

	// Publish duplicate resource type and verify TopicSet deduplication.
	_ = pub.Publish(context.Background(), []cdc.Message{
		{TenantID: "tenant-a", ResType: "Patient", ResID: "pat-2", Op: "U"},
	})
	topics = cdc.TopicSet(pub.Messages())
	if len(topics) != 2 {
		t.Fatalf("expected deduplicated TopicSet of length 2, got %v", topics)
	}

	pub.Reset()
	if got := len(pub.Messages()); got != 0 {
		t.Fatalf("expected 0 messages after Reset, got %d", got)
	}
}

func TestMemoryPublisher_ConcurrentPublish(t *testing.T) {
	pub := cdc.NewMemoryPublisher()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			_ = pub.Publish(context.Background(), []cdc.Message{
				{TenantID: "t", ResType: "Patient", ResID: "p", ProtoData: []byte{byte(id)}, Op: "U"},
			})
			_ = pub.Messages()
		}(i)
	}
	wg.Wait()
	if got := len(pub.Messages()); got != 16 {
		t.Fatalf("expected 16 messages after concurrent publish, got %d", got)
	}
}
