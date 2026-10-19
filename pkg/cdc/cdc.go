// Package cdc defines the generic Change Data Capture (CDC) event publishing
// interface for the Flint FHIR R4 server.
//
// By default, the FHIR server runs with no CDC publisher configured (nil).
// Optional add-on adapters (such as package automq for Kafka/AutoMQ & Apache Iceberg)
// implement the Publisher interface and can be wired into the server at startup.
package cdc

import (
	"context"
	"sync"
)

// Message represents a single FHIR resource mutation event to publish.
type Message struct {
	TenantID  string
	ResType   string // e.g., "Patient"
	ResID     string // used as partition key
	ProtoData []byte // proto-encoded resource
	Op        string // "U" (upsert) or "D" (delete)
}

// Publisher publishes FHIR resource CDC messages to an external streaming sink or in-memory test sink.
type Publisher interface {
	Publish(ctx context.Context, messages []Message) error
}

// MemoryPublisher is a thread-safe in-memory Publisher for unit and integration testing.
type MemoryPublisher struct {
	mu       sync.RWMutex
	messages []Message
}

// NewMemoryPublisher creates a new in-memory CDC publisher.
func NewMemoryPublisher() *MemoryPublisher {
	return &MemoryPublisher{}
}

// Publish records CDC messages in memory.
func (m *MemoryPublisher) Publish(_ context.Context, messages []Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, msg := range messages {
		cp := msg
		if len(msg.ProtoData) > 0 {
			cp.ProtoData = append([]byte(nil), msg.ProtoData...)
		}
		m.messages = append(m.messages, cp)
	}
	return nil
}

// Messages returns a snapshot of all published CDC messages.
func (m *MemoryPublisher) Messages() []Message {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Message, len(m.messages))
	copy(out, m.messages)
	return out
}

// Reset clears all recorded CDC messages.
func (m *MemoryPublisher) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = nil
}

// TopicSet returns the deduplicated list of resource types in msgs.
func TopicSet(msgs []Message) []string {
	seen := map[string]bool{}
	var topics []string
	for _, m := range msgs {
		if !seen[m.ResType] {
			seen[m.ResType] = true
			topics = append(topics, m.ResType)
		}
	}
	return topics
}
