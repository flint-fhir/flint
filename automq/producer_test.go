package automq_test

import (
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
			name: "Patient upsert",
			msg: automq.Message{
				TenantID:  "tenant_a",
				ResType:   "Patient",
				ResID:     "pat-100",
				ProtoData: []byte("proto-payload-bytes"),
				Op:        "U",
			},
			wantTopic:    "fhir.Patient",
			wantKey:      "pat-100",
			wantOp:       "U",
			wantTenantID: "tenant_a",
		},
		{
			name: "Observation delete tombstone",
			msg: automq.Message{
				TenantID:  "tenant_b",
				ResType:   "Observation",
				ResID:     "obs-555",
				ProtoData: nil,
				Op:        "D",
			},
			wantTopic:    "fhir.Observation",
			wantKey:      "obs-555",
			wantOp:       "D",
			wantTenantID: "tenant_b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := p.BuildRecord(tt.msg)
			require.NotNil(t, record)

			assert.Equal(t, tt.wantTopic, record.Topic)
			assert.Equal(t, tt.wantKey, string(record.Key))
			assert.Equal(t, tt.msg.ProtoData, record.Value)

			// Verify CDC Headers
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
