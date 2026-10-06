package main

import (
	"context"
	"fmt"
	"log"

	"github.com/flint-fhir/flint/automq"
)

func runDelete() {
	ctx := context.Background()
	brokers := []string{envOrDefault("AUTOMQ_BROKERS", "localhost:9092")}

	p, err := automq.NewProducer(automq.Config{
		Brokers:     brokers,
		TopicPrefix: "fhir.",
	})
	if err != nil {
		log.Fatalf("producer: %v", err)
	}
	defer p.Close()

	// Publish delete event for Charlie (e2e-wf-003)
	msg := automq.Message{
		TenantID:  "e2e-test",
		ResType:   "Patient",
		ResID:     "e2e-wf-003",
		ProtoData: []byte{}, // tombstone payload
		Op:        "D",      // Delete CDC operation
	}

	fmt.Println("🚀 Publishing delete CDC event (op=D) for Patient e2e-wf-003...")
	if err := p.Publish(ctx, []automq.Message{msg}); err != nil {
		log.Fatalf("publish: %v", err)
	}
	fmt.Println("✅ Delete CDC event published successfully!")
}
