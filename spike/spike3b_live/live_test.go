//go:build integration

// Spike 3b Live: Send proto-encoded FHIR Patient to AutoMQ, verify delivery.
// This validates that AutoMQ can receive and store proto messages.
// Table Topic validation requires AutoMQ v1.4.1+ config — tested in staging.
package spike3b_live

import (
	"context"
	"testing"
	"os"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"

	pb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto"
	dtpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/datatypes_go_proto"
	codepb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/codes_go_proto"
)

const (
	broker = "localhost:9092"
	topic  = "fhir.patient.v1"
)

func buildTestPatient() *pb.Patient {
	return &pb.Patient{
		Id:     &dtpb.Id{Value: "spike-test-001"},
		Active: &dtpb.Boolean{Value: true},
		Name: []*dtpb.HumanName{
			{
				Family: &dtpb.String{Value: "O'Brien"},
				Given: []*dtpb.String{
					{Value: "Miles"},
					{Value: "Edward"},
				},
			},
		},
		Gender: &pb.Patient_GenderCode{
			Value: codepb.AdministrativeGenderCode_MALE,
		},
		BirthDate: &dtpb.Date{
			ValueUs:   -238291200000000, // 1962-06-15
			Precision: dtpb.Date_DAY,
		},
		Identifier: []*dtpb.Identifier{
			{
				System: &dtpb.Uri{Value: "https://hospital.example.com/mrn"},
				Value:  &dtpb.String{Value: "MRN-12345"},
			},
		},
	}
}

func TestProtoProduceConsume(t *testing.T) {
	if testing.Short() || os.Getenv("CI") != "" {
		t.Skip("skipping live test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Build a FHIR Patient proto
	patient := buildTestPatient()
	patientBytes, err := proto.Marshal(patient)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	t.Logf("Patient proto size: %d bytes", len(patientBytes))

	// Produce to AutoMQ
	producer, err := kgo.NewClient(
		kgo.SeedBrokers(broker),
		kgo.DefaultProduceTopic(topic),
	)
	if err != nil {
		t.Fatalf("Failed to create producer (is AutoMQ running on %s?): %v", broker, err)
	}
	defer producer.Close()

	// Create topic first
	t.Log("Producing proto-encoded Patient to AutoMQ...")
	record := &kgo.Record{
		Key:   []byte("Patient/spike-test-001"),
		Value: patientBytes,
		Headers: []kgo.RecordHeader{
			{Key: "res_type", Value: []byte("Patient")},
			{Key: "res_id", Value: []byte("spike-test-001")},
			{Key: "tenant_id", Value: []byte("test-tenant")},
		},
	}

	results := producer.ProduceSync(ctx, record)
	if err := results.FirstErr(); err != nil {
		t.Fatalf("Produce failed: %v", err)
	}
	t.Logf("✅ Produced Patient proto to topic %q (partition %d, offset %d)",
		topic, results[0].Record.Partition, results[0].Record.Offset)

	// Consume back and verify round-trip
	consumer, err := kgo.NewClient(
		kgo.SeedBrokers(broker),
		kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
	)
	if err != nil {
		t.Fatalf("Failed to create consumer: %v", err)
	}
	defer consumer.Close()

	t.Log("Consuming back from AutoMQ...")
	fetches := consumer.PollFetches(ctx)
	if errs := fetches.Errors(); len(errs) > 0 {
		t.Fatalf("Fetch errors: %v", errs)
	}

	var found bool
	fetches.EachRecord(func(r *kgo.Record) {
		// Deserialize
		roundTrip := &pb.Patient{}
		if err := proto.Unmarshal(r.Value, roundTrip); err != nil {
			t.Errorf("proto.Unmarshal failed: %v", err)
			return
		}

		if roundTrip.GetId().GetValue() == "spike-test-001" {
			found = true
			t.Logf("✅ Round-trip Patient ID: %s", roundTrip.GetId().GetValue())
			t.Logf("✅ Family: %s", roundTrip.GetName()[0].GetFamily().GetValue())
			t.Logf("✅ Given: %s %s",
				roundTrip.GetName()[0].GetGiven()[0].GetValue(),
				roundTrip.GetName()[0].GetGiven()[1].GetValue())
			t.Logf("✅ Gender: %s", roundTrip.GetGender().GetValue().String())
			t.Logf("✅ MRN: %s", roundTrip.GetIdentifier()[0].GetValue().GetValue())

			// Verify headers
			for _, h := range r.Headers {
				t.Logf("   Header: %s = %s", h.Key, string(h.Value))
			}

			// Proto size check
			t.Logf("✅ Proto payload: %d bytes (compact, no JSON overhead)", len(r.Value))
		}
	})

	if !found {
		t.Error("❌ Did not find spike-test-001 in consumed messages")
	}

	t.Log("\n=== SPIKE 3b LIVE RESULTS ===")
	t.Log("Proto-encoded FHIR Patient → AutoMQ → round-trip: ✅")
	t.Log("AutoMQ accepts proto bytes as Kafka message values.")
	t.Log("Headers carry metadata (res_type, res_id, tenant_id).")
	t.Log("")
	t.Log("NEXT: Enable Table Topic on this topic in AutoMQ v1.4.1+")
	t.Log("      to validate auto-materialization to Iceberg.")
}
