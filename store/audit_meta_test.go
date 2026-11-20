package store_test

import (
	"testing"
	"time"

	"github.com/flint-fhir/flint/store"
)

func TestAuditOutcomeMetadataRoundTrip(t *testing.T) {
	in := store.AuditRecord{
		TenantID:            "hospital-a",
		AuditID:             "aud-meta-1",
		Recorded:            time.Now().UTC(),
		Action:              "C",
		SubtypeCode:         "create",
		Outcome:             "0",
		OutcomeDesc:         "201 Created [custom bracket]",
		AgentSubject:        "svc-gw",
		AgentIssuer:         "https://idp.azra.example/realms/main [v1]",
		ActorSubject:        "dr jane",
		ActorIssuer:         "https://zitadel.azra.example",
		OperatorCrossTenant: true,
		PurposeOfUse:        "TREAT",
		RequestID:           "req [123] abc",
	}

	encoded := store.EncodeAuditOutcomeDesc(in)
	reencoded := store.EncodeAuditOutcomeDesc(store.AuditRecord{
		OutcomeDesc:         encoded,
		AgentIssuer:         in.AgentIssuer,
		ActorSubject:        in.ActorSubject,
		ActorIssuer:         in.ActorIssuer,
		OperatorCrossTenant: in.OperatorCrossTenant,
		PurposeOfUse:        in.PurposeOfUse,
		RequestID:           in.RequestID,
	})
	if encoded != reencoded {
		t.Fatalf("EncodeAuditOutcomeDesc is not idempotent:\nfirst:  %q\nsecond: %q", encoded, reencoded)
	}

	var decoded store.AuditRecord
	decoded.OutcomeDesc = encoded
	store.HydrateAuditOutcomeMetadata(&decoded)

	if decoded.OutcomeDesc != in.OutcomeDesc {
		t.Fatalf("OutcomeDesc mismatch: got %q want %q", decoded.OutcomeDesc, in.OutcomeDesc)
	}
	if decoded.PurposeOfUse != in.PurposeOfUse {
		t.Fatalf("PurposeOfUse mismatch: got %q want %q", decoded.PurposeOfUse, in.PurposeOfUse)
	}
	if decoded.AgentIssuer != in.AgentIssuer {
		t.Fatalf("AgentIssuer mismatch: got %q want %q", decoded.AgentIssuer, in.AgentIssuer)
	}
	if decoded.ActorSubject != in.ActorSubject {
		t.Fatalf("ActorSubject mismatch: got %q want %q", decoded.ActorSubject, in.ActorSubject)
	}
	if decoded.ActorIssuer != in.ActorIssuer {
		t.Fatalf("ActorIssuer mismatch: got %q want %q", decoded.ActorIssuer, in.ActorIssuer)
	}
	if !decoded.OperatorCrossTenant {
		t.Fatalf("expected OperatorCrossTenant=true")
	}
	if decoded.RequestID != in.RequestID {
		t.Fatalf("RequestID mismatch: got %q want %q", decoded.RequestID, in.RequestID)
	}
}
