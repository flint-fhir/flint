package audit

import (
	"testing"
	"time"

	"github.com/flint-fhir/flint/store"
)

func TestToFHIRResourceExtendedAttributionAndDigest(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	base := store.AuditRecord{
		TenantID:            "t-a",
		AuditID:             "aud-attr-1",
		Recorded:            now,
		Action:              "C",
		SubtypeCode:         "create",
		Outcome:             "0",
		OutcomeDesc:         "201 Created",
		HTTPMethod:          "POST",
		HTTPStatus:          201,
		RequestURI:          "/fhir/r4/Patient",
		AgentSubject:        "svc-gw",
		AgentIssuer:         "https://workload.azra.internal",
		ActorSubject:        "dr-alice",
		ActorIssuer:         "https://zitadel.azra.example",
		OperatorCrossTenant: true,
		RequestID:           "req-xyz",
		EntityType:          "Patient",
		EntityID:            "p1",
		PurposeOfUse:        "TREAT",
	}

	res := ToFHIRResource(base)
	agents, ok := res["agent"].([]map[string]any)
	if !ok || len(agents) != 2 {
		t.Fatalf("expected 2 agents (caller + delegated actor), got %#v", res["agent"])
	}
	primaryIdent := agents[0]["who"].(map[string]any)["identifier"].(map[string]any)
	if primaryIdent["value"] != "svc-gw" || primaryIdent["system"] != "https://workload.azra.internal" {
		t.Fatalf("unexpected primary agent identifier: %+v", primaryIdent)
	}
	delegatedIdent := agents[1]["who"].(map[string]any)["identifier"].(map[string]any)
	if delegatedIdent["value"] != "dr-alice" || delegatedIdent["system"] != "https://zitadel.azra.example" || agents[1]["requestor"] != false {
		t.Fatalf("unexpected delegated actor agent: %+v", agents[1])
	}

	d1 := ComputeRecordDigest(base)
	modified := base
	modified.ActorSubject = "dr-mallory"
	d2 := ComputeRecordDigest(modified)
	if d1 == d2 {
		t.Fatalf("ComputeRecordDigest must cover ActorSubject changes")
	}
}
