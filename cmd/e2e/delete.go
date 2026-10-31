package main

import (
	"context"
	"fmt"

	"github.com/flint-fhir/flint/automq"
)

func buildDeleteCDCMessage() automq.Message {
	return automq.Message{
		TenantID:  "e2e-test",
		ResType:   "Patient",
		ResID:     "e2e-wf-003",
		ProtoData: []byte{}, // tombstone payload
		Op:        "D",      // Delete CDC operation
	}
}

func runDeleteWithDeps(ctx context.Context, deps e2eDeps) error {
	brokers := []string{envOrDefault("AUTOMQ_BROKERS", "localhost:9092")}
	msg := buildDeleteCDCMessage()

	fmt.Println("🚀 Publishing delete CDC event (op=D) for Patient e2e-wf-003...")
	if err := deps.publishCDC(ctx, brokers, "", []automq.Message{msg}); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	fmt.Println("✅ Delete CDC event published successfully!")
	return nil
}
