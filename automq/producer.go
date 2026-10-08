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

	"github.com/twmb/franz-go/pkg/kgo"
)

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

// BuildRecord creates a kafka Record from a Message, attaching CDC headers and partition key.
func (p *Producer) BuildRecord(msg Message) *kgo.Record {
	topic := p.TopicPrefix + msg.ResType
	return &kgo.Record{
		Topic: topic,
		Key:   []byte(msg.ResID),
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
// Messages are keyed by ResID for partition locality.
func (p *Producer) Publish(ctx context.Context, messages []Message) error {
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

	p.logger.Info("published to automq",
		"count", len(messages),
		"topics", topicSet(messages),
	)
	return nil
}

// Close shuts down the producer.
func (p *Producer) Close() {
	p.client.Close()
}

func topicSet(msgs []Message) []string {
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
