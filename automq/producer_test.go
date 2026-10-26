package automq_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/automq"
)

func TestProducer_BuildRecord(t *testing.T) {
	p := &automq.Producer{
		TopicPrefix: "fhir.",
	}

	tests := []struct {
		name         string
		msg          automq.Message
		wantTopic    string
		wantKey      string
		wantOp       string
		wantTenantID string
	}{
		{
			name: "Patient upsert with tenant",
			msg: automq.Message{
				TenantID:  "tenant_a",
				ResType:   "Patient",
				ResID:     "pat-100",
				ProtoData: []byte("proto-payload-bytes"),
				Op:        "U",
			},
			wantTopic:    "fhir.Patient",
			wantKey:      "tenant_a/pat-100",
			wantOp:       "U",
			wantTenantID: "tenant_a",
		},
		{
			name: "Observation delete tombstone with tenant",
			msg: automq.Message{
				TenantID:  "tenant_b",
				ResType:   "Observation",
				ResID:     "obs-555",
				ProtoData: nil,
				Op:        "D",
			},
			wantTopic:    "fhir.Observation",
			wantKey:      "tenant_b/obs-555",
			wantOp:       "D",
			wantTenantID: "tenant_b",
		},
		{
			name: "Resource without tenant falls back to bare ResID",
			msg: automq.Message{
				TenantID:  "",
				ResType:   "Condition",
				ResID:     "cond-999",
				ProtoData: []byte("proto"),
				Op:        "U",
			},
			wantTopic:    "fhir.Condition",
			wantKey:      "cond-999",
			wantOp:       "U",
			wantTenantID: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := p.BuildRecord(tt.msg)
			require.NotNil(t, record)

			assert.Equal(t, tt.wantTopic, record.Topic)
			assert.Equal(t, tt.wantKey, string(record.Key))
			assert.Equal(t, tt.msg.ProtoData, record.Value)

			headerMap := make(map[string]string)
			for _, h := range record.Headers {
				headerMap[h.Key] = string(h.Value)
			}

			assert.Equal(t, tt.wantTenantID, headerMap["tenant_id"])
			assert.Equal(t, tt.msg.ResType, headerMap["res_type"])
			assert.Equal(t, tt.msg.ResID, headerMap["res_id"])
			assert.Equal(t, tt.wantOp, headerMap["op"])
		})
	}
}

func TestProducer_ApicurioSchemaRegistryAndFraming(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/apis/ccompat/v7/subjects/fhir.Patient-value/versions", r.URL.Path)
		assert.Equal(t, "application/vnd.schemaregistry.v1+json", r.Header.Get("Content-Type"))

		var reqBody map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&reqBody))
		assert.Equal(t, "PROTOBUF", reqBody["schemaType"])
		assert.Equal(t, automq.ResourceEnvelopeProtoSchema, reqBody["schema"])

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":7}`))
	}))
	defer srv.Close()

	p := &automq.Producer{
		TopicPrefix:       "fhir.",
		SchemaRegistryURL: srv.URL + "/apis/ccompat/v7",
	}

	id1, err := p.EnsureSchemaID(context.Background(), "fhir.Patient")
	require.NoError(t, err)
	assert.Equal(t, uint32(7), id1)

	// Second call hits in-memory cache without another HTTP request
	id2, err := p.EnsureSchemaID(context.Background(), "fhir.Patient")
	require.NoError(t, err)
	assert.Equal(t, uint32(7), id2)
	assert.Equal(t, int32(1), calls.Load())

	msg := automq.Message{
		TenantID:  "tenant_a",
		ResType:   "Patient",
		ResID:     "pat-001",
		ProtoData: []byte(`{"resourceType":"Patient","id":"pat-001"}`),
		Op:        "U",
	}
	rec := p.BuildRecord(msg)
	gotID, rawProto, err := automq.UnwrapConfluentProtobuf(rec.Value)
	require.NoError(t, err)
	assert.Equal(t, uint32(7), gotID)
	assert.Equal(t, automq.EncodeResourceEnvelopeProto(msg), rawProto)
}

func TestTopicSet(t *testing.T) {
	topics := automq.TopicSet([]automq.Message{
		{ResType: "Patient"},
		{ResType: "Observation"},
		{ResType: "Patient"},
		{ResType: "Encounter"},
	})
	assert.Equal(t, []string{"Patient", "Observation", "Encounter"}, topics)
}

func TestMemoryPublisher(t *testing.T) {
	pub := automq.NewMemoryPublisher()
	require.Empty(t, pub.Messages())

	err := pub.Publish(context.Background(), []automq.Message{
		{
			TenantID:  "tenant_1",
			ResType:   "Patient",
			ResID:     "pat-1",
			ProtoData: []byte{1, 2, 3},
			Op:        "U",
		},
		{
			TenantID: "tenant_1",
			ResType:  "Patient",
			ResID:    "pat-1",
			Op:       "D",
		},
	})
	require.NoError(t, err)

	msgs := pub.Messages()
	require.Len(t, msgs, 2)
	assert.Equal(t, "U", msgs[0].Op)
	assert.Equal(t, "D", msgs[1].Op)

	pub.Reset()
	assert.Empty(t, pub.Messages())
}

func TestNewProducer(t *testing.T) {
	_, err := automq.NewProducer(automq.Config{})
	require.Error(t, err)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	prod, err := automq.NewProducer(automq.Config{
		Brokers:     []string{"127.0.0.1:19092"},
		TopicPrefix: "fhir.",
		Logger:      logger,
	})
	require.NoError(t, err)
	require.NotNil(t, prod)
	assert.Equal(t, "fhir.", prod.TopicPrefix)
	require.NoError(t, prod.Publish(context.Background(), nil))
	prod.Close()
}
