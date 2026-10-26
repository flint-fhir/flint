// Package automq provides a Kafka/AutoMQ producer for publishing
// proto-encoded FHIR resources to per-resource-type topics.
//
// AutoMQ is wire-compatible with Kafka, so we use franz-go as the client.
// Each resource type gets its own topic (e.g., "fhir.Patient", "fhir.Encounter").
// Messages use the resource ID as the key (for partition locality and compaction)
// and support both raw proto bytes and Confluent Schema Registry / Apicurio
// (by_schema_id) Protobuf wire framing for AutoMQ Table Topics -> Apache Iceberg.
package automq

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/flint-fhir/flint/pkg/cdc"
)

// ResourceEnvelopeProtoSchema is the canonical Protobuf schema registered per
// <topic>-value subject in Apicurio Schema Registry (/apis/ccompat/v7) for
// AutoMQ Table Topics (automq.table.topic.convert.value.type=by_schema_id,
// automq.table.topic.transform.value.type=flatten).
const ResourceEnvelopeProtoSchema = `syntax = "proto3";
package flint.fhir;

message ResourceEnvelope {
  string res_id = 1;
  string res_type = 2;
  string tenant_id = 3;
  string payload = 4;
  string op = 5;
}
`

// Publisher is an alias for cdc.Publisher so automq.Producer satisfies the server's generic CDC interface.
type Publisher = cdc.Publisher

// Message is an alias for cdc.Message.
type Message = cdc.Message

// MemoryPublisher is an alias for cdc.MemoryPublisher.
type MemoryPublisher = cdc.MemoryPublisher

// NewMemoryPublisher creates a new in-memory CDC publisher.
var NewMemoryPublisher = cdc.NewMemoryPublisher

// Producer publishes FHIR resource protos to AutoMQ/Kafka topics.
type Producer struct {
	client *kgo.Client
	logger *slog.Logger
	// TopicPrefix is prepended to resource type names (e.g., "fhir." -> "fhir.Patient")
	TopicPrefix string
	// SchemaRegistryURL is the optional Apicurio / Confluent Schema Registry v7 base URL
	// (e.g., "http://apicurio:8080/apis/ccompat/v7"). When set, Producer registers
	// ResourceEnvelopeProtoSchema under <topic>-value and frames records with the
	// 6-byte Confluent Protobuf wire header for AutoMQ by_schema_id Table Topics.
	SchemaRegistryURL string
	// DefaultSchemaID is an optional static Confluent/Apicurio schema ID to frame
	// records with when SchemaRegistryURL is not queried dynamically.
	DefaultSchemaID uint32

	httpClient *http.Client
	schemaMu   sync.RWMutex
	schemaIDs  map[string]uint32
}

// Config holds AutoMQ/Kafka producer configuration.
type Config struct {
	Brokers           []string // e.g., ["localhost:9092"]
	TopicPrefix       string   // e.g., "fhir."
	SchemaRegistryURL string   // optional, e.g., "http://apicurio:8080/apis/ccompat/v7"
	DefaultSchemaID   uint32   // optional static schema ID for by_schema_id framing
	Logger            *slog.Logger
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
		client:            client,
		logger:            logger,
		TopicPrefix:       cfg.TopicPrefix,
		SchemaRegistryURL: strings.TrimRight(strings.TrimSpace(cfg.SchemaRegistryURL), "/"),
		DefaultSchemaID:   cfg.DefaultSchemaID,
		httpClient:        &http.Client{Timeout: 10 * time.Second},
		schemaIDs:         make(map[string]uint32),
	}, nil
}

// EncodeResourceEnvelopeProto encodes a CDC Message into the wire-format bytes
// of flint.fhir.ResourceEnvelope (fields 1..5: res_id, res_type, tenant_id, payload, op).
func EncodeResourceEnvelopeProto(msg Message) []byte {
	var b []byte
	if msg.ResID != "" {
		b = protowire.AppendTag(b, 1, protowire.BytesType)
		b = protowire.AppendString(b, msg.ResID)
	}
	if msg.ResType != "" {
		b = protowire.AppendTag(b, 2, protowire.BytesType)
		b = protowire.AppendString(b, msg.ResType)
	}
	if msg.TenantID != "" {
		b = protowire.AppendTag(b, 3, protowire.BytesType)
		b = protowire.AppendString(b, msg.TenantID)
	}
	if len(msg.ProtoData) > 0 {
		b = protowire.AppendTag(b, 4, protowire.BytesType)
		b = protowire.AppendBytes(b, msg.ProtoData)
	}
	if msg.Op != "" {
		b = protowire.AppendTag(b, 5, protowire.BytesType)
		b = protowire.AppendString(b, msg.Op)
	}
	return b
}

// WrapConfluentProtobuf prepends the 6-byte Confluent Schema Registry Protobuf wire header:
//   - Byte 0: Magic byte 0x00
//   - Bytes 1..4: 4-byte big-endian Schema ID
//   - Byte 5: Message index array length = 0 (0x00, first message in .proto file)
func WrapConfluentProtobuf(schemaID uint32, protoBytes []byte) []byte {
	out := make([]byte, 6+len(protoBytes))
	out[0] = 0x00
	binary.BigEndian.PutUint32(out[1:5], schemaID)
	out[5] = 0x00
	copy(out[6:], protoBytes)
	return out
}

// UnwrapConfluentProtobuf parses the 6-byte Confluent Schema Registry Protobuf wire header.
func UnwrapConfluentProtobuf(framed []byte) (uint32, []byte, error) {
	if len(framed) < 6 {
		return 0, nil, fmt.Errorf("automq.UnwrapConfluentProtobuf: frame too short (%d bytes)", len(framed))
	}
	if framed[0] != 0x00 {
		return 0, nil, fmt.Errorf("automq.UnwrapConfluentProtobuf: invalid magic byte 0x%02x (expected 0x00)", framed[0])
	}
	schemaID := binary.BigEndian.Uint32(framed[1:5])
	if framed[5] != 0x00 {
		return 0, nil, fmt.Errorf("automq.UnwrapConfluentProtobuf: unsupported message index byte 0x%02x", framed[5])
	}
	return schemaID, framed[6:], nil
}

// EnsureSchemaID registers ResourceEnvelopeProtoSchema under <topic>-value in Apicurio
// (Confluent compat v7 API) if not already cached, and returns the assigned schema ID.
func (p *Producer) EnsureSchemaID(ctx context.Context, topic string) (uint32, error) {
	p.schemaMu.RLock()
	if id, ok := p.schemaIDs[topic]; ok && id > 0 {
		p.schemaMu.RUnlock()
		return id, nil
	}
	p.schemaMu.RUnlock()

	baseURL := strings.TrimRight(strings.TrimSpace(p.SchemaRegistryURL), "/")
	if baseURL == "" {
		return p.DefaultSchemaID, nil
	}

	p.schemaMu.Lock()
	defer p.schemaMu.Unlock()
	if id, ok := p.schemaIDs[topic]; ok && id > 0 {
		return id, nil
	}

	reqBody, err := json.Marshal(map[string]string{
		"schemaType": "PROTOBUF",
		"schema":     ResourceEnvelopeProtoSchema,
	})
	if err != nil {
		return 0, fmt.Errorf("automq.EnsureSchemaID marshal: %w", err)
	}

	subject := topic + "-value"
	endpoint := fmt.Sprintf("%s/subjects/%s/versions", baseURL, subject)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return 0, fmt.Errorf("automq.EnsureSchemaID request: %w", err)
	}
	req.Header.Set("Content-Type", "application/vnd.schemaregistry.v1+json")

	httpClient := p.httpClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("automq.EnsureSchemaID post %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("automq.EnsureSchemaID status=%d body=%s", resp.StatusCode, string(body))
	}

	var parsed struct {
		ID uint32 `json:"id"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.ID == 0 {
		return 0, fmt.Errorf("automq.EnsureSchemaID invalid response %q: %w", string(body), err)
	}

	if p.schemaIDs == nil {
		p.schemaIDs = make(map[string]uint32)
	}
	p.schemaIDs[topic] = parsed.ID
	return parsed.ID, nil
}

func (p *Producer) cachedSchemaID(topic string) uint32 {
	p.schemaMu.RLock()
	if p.schemaIDs != nil {
		if id, ok := p.schemaIDs[topic]; ok && id > 0 {
			p.schemaMu.RUnlock()
			return id
		}
	}
	p.schemaMu.RUnlock()
	return p.DefaultSchemaID
}

// BuildRecord creates a kafka Record from a Message, attaching CDC headers and tenant-scoped partition key.
// When a Confluent/Apicurio schema ID is configured for the topic, the record value is framed
// using Confluent Protobuf wire format (by_schema_id) wrapping ResourceEnvelope.
func (p *Producer) BuildRecord(msg Message) *kgo.Record {
	topic := p.TopicPrefix + msg.ResType
	key := msg.ResID
	if msg.TenantID != "" {
		key = msg.TenantID + "/" + msg.ResID
	}

	val := msg.ProtoData
	if schemaID := p.cachedSchemaID(topic); schemaID > 0 {
		val = WrapConfluentProtobuf(schemaID, EncodeResourceEnvelopeProto(msg))
	}

	return &kgo.Record{
		Topic: topic,
		Key:   []byte(key),
		Value: val,
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

	if strings.TrimSpace(p.SchemaRegistryURL) != "" {
		for _, resType := range TopicSet(messages) {
			topic := p.TopicPrefix + resType
			if _, err := p.EnsureSchemaID(ctx, topic); err != nil {
				return fmt.Errorf("automq.Publish ensure schema topic=%s: %w", topic, err)
			}
		}
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
