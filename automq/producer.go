// Package automq provides a Kafka/AutoMQ producer for publishing
// proto-encoded FHIR resources to per-resource-type topics.
//
// AutoMQ is wire-compatible with Kafka, so we use franz-go as the client.
// Each resource type gets its own topic (e.g., "fhir.Patient", "fhir.Encounter").
// Messages use the resource ID as the key (for partition locality and compaction)
// and proto-encoded bytes as the value, with headers for tenant and operation.
package automq

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/twmb/franz-go/pkg/kgo"
)

// Publisher publishes FHIR resource CDC messages to AutoMQ/Kafka or an in-memory test sink.
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

// Producer publishes FHIR resource protos to AutoMQ/Kafka topics.
type Producer struct {
	client *kgo.Client
	logger *slog.Logger
	// TopicPrefix is prepended to resource type names (e.g., "fhir." → "fhir.Patient")
	TopicPrefix string
}

// Config holds AutoMQ/Kafka producer configuration.
type Config struct {
	Brokers     []string // e.g., ["localhost:9092"]
	TopicPrefix string   // e.g., "fhir."
	Logger      *slog.Logger
}

// NewProducer creates a new AutoMQ producer connected to the given brokers.
func NewProducer(cfg Config) (*Producer, error) {
	if len(cfg.Brokers) == 0 {
		return nil, fmt.Errorf("automq.NewProducer: at least one broker is required")
	}
	client, err := kgo.NewClient(
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.AllowAutoTopicCreation(),
		kgo.ProducerBatchCompression(kgo.SnappyCompression()),
	)
	if err != nil {
		return nil, fmt.Errorf("automq.NewProducer: %w", err)
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Producer{
		client:      client,
		logger:      logger,
		TopicPrefix: cfg.TopicPrefix,
	}, nil
}

// Message represents a single FHIR resource to publish.
type Message struct {
	TenantID  string
	ResType   string // e.g., "Patient"
	ResID     string // used as partition key
	ProtoData []byte // proto-encoded resource
	Op        string // "U" (upsert) or "D" (delete)
}

// BuildRecord creates a kafka Record from a Message, attaching CDC headers and tenant-scoped partition key.
func (p *Producer) BuildRecord(msg Message) *kgo.Record {
	topic := p.TopicPrefix + msg.ResType
	key := msg.ResID
	if msg.TenantID != "" {
		key = msg.TenantID + "/" + msg.ResID
	}
	return &kgo.Record{
		Topic: topic,
		Key:   []byte(key),
		Value: msg.ProtoData,
		Headers: []kgo.RecordHeader{
			{Key: "tenant_id", Value: []byte(msg.TenantID)},
			{Key: "res_type", Value: []byte(msg.ResType)},
			{Key: "res_id", Value: []byte(msg.ResID)},
			{Key: "op", Value: []byte(msg.Op)},
		},
	}
}

// Publish sends a batch of FHIR resource messages to their respective topics.
// Each resource type maps to a separate topic (TopicPrefix + ResType).
// Messages are keyed by TenantID/ResID for tenant-isolated partition locality and compaction.
func (p *Producer) Publish(ctx context.Context, messages []Message) error {
	if len(messages) == 0 {
		return nil
	}
	if p == nil || p.client == nil {
		return fmt.Errorf("automq.Publish: producer client is nil")
	}
	records := make([]*kgo.Record, len(messages))
	for i, msg := range messages {
		records[i] = p.BuildRecord(msg)
	}

	results := p.client.ProduceSync(ctx, records...)
	for _, result := range results {
		if result.Err != nil {
			return fmt.Errorf("automq.Publish topic=%s key=%s: %w",
				result.Record.Topic, string(result.Record.Key), result.Err)
		}
	}

	if p.logger != nil {
		p.logger.Info("published to automq",
			"count", len(messages),
			"topics", TopicSet(messages),
		)
	}
	return nil
}

// Close shuts down the producer.
func (p *Producer) Close() {
	if p != nil && p.client != nil {
		p.client.Close()
	}
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
