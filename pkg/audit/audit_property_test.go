package audit_test

import (
	"strings"
	"testing"
	"time"

	"hegel.dev/go/hegel"

	"github.com/flint-fhir/flint/pkg/audit"
	"github.com/flint-fhir/flint/store/postgres"
)

func TestProperty_OutcomeClassificationInvariance(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		status := hegel.Draw(ht, hegel.Integers(100, 599))
		outcome, desc := audit.ClassifyHTTPOutcome(status)

		if desc == "" {
			ht.Fatalf("expected non-empty outcomeDesc for status %d", status)
		}
		switch {
		case status < 400:
			if outcome != audit.OutcomeSuccess {
				ht.Fatalf("status %d: expected outcome %q, got %q", status, audit.OutcomeSuccess, outcome)
			}
		case status < 500:
			if outcome != audit.OutcomeMinorFailure {
				ht.Fatalf("status %d: expected outcome %q, got %q", status, audit.OutcomeMinorFailure, outcome)
			}
		default:
			if outcome != audit.OutcomeSeriousFailure {
				ht.Fatalf("status %d: expected outcome %q, got %q", status, audit.OutcomeSeriousFailure, outcome)
			}
		}
	})
}

func TestProperty_InteractionClassificationAndFHIRSerialization(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		method := hegel.Draw(ht, hegel.FromRegex(`(GET|POST|PUT|DELETE)`, true))
		tenant := hegel.Draw(ht, hegel.FromRegex(`[a-z]{3,8}`, true))
		resType := hegel.Draw(ht, hegel.FromRegex(`(Patient|Observation|Condition|Encounter|Practitioner)`, true))
		resID := hegel.Draw(ht, hegel.FromRegex(`[a-z0-9\-]{3,12}`, true))
		status := hegel.Draw(ht, hegel.Integers(200, 503))

		parts := []string{"fhir", "r4", tenant, resType, resID}
		act, sub, eType, eID, eVer := audit.ClassifyHTTPInteraction(method, parts, status)

		switch act {
		case audit.ActionCreate, audit.ActionRead, audit.ActionUpdate, audit.ActionDelete, audit.ActionExecute:
		default:
			ht.Fatalf("invalid FHIR AuditEventAction %q for %s %v", act, method, parts)
		}
		if sub == "" {
			ht.Fatalf("empty subtype for %s %v", method, parts)
		}

		outcome, desc := audit.ClassifyHTTPOutcome(status)
		fhirRes := audit.ToFHIRResource(postgres.AuditRecord{
			TenantID:      tenant,
			AuditID:       "prop-audit-1",
			Recorded:      time.Now().UTC(),
			Action:        act,
			SubtypeCode:   sub,
			Outcome:       outcome,
			OutcomeDesc:   desc,
			HTTPMethod:    method,
			HTTPStatus:    status,
			RequestURI:    "/" + strings.Join(parts, "/"),
			AgentSubject:  "prop-user",
			EntityType:    eType,
			EntityID:      eID,
			EntityVersion: eVer,
		})

		if fhirRes["resourceType"] != "AuditEvent" {
			ht.Fatalf("expected resourceType AuditEvent, got %v", fhirRes["resourceType"])
		}
		if fhirRes["action"] != act || fhirRes["outcome"] != outcome {
			ht.Fatalf("FHIR AuditEvent mismatch: action=%v outcome=%v", fhirRes["action"], fhirRes["outcome"])
		}
	})
}

func TestProperty_ClassifyNeverPanicsOnArbitraryFuzz(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		method := hegel.Draw(ht, hegel.FromRegex(`.*`, true))
		rawPath := hegel.Draw(ht, hegel.FromRegex(`.*`, true))
		status := hegel.Draw(ht, hegel.Integers(-100, 1000))

		parts := strings.Split(strings.Trim(rawPath, "/"), "/")
		act, sub, eType, eID, eVer := audit.ClassifyHTTPInteraction(method, parts, status)
		out, desc := audit.ClassifyHTTPOutcome(status)
		_ = audit.ToFHIRResource(postgres.AuditRecord{
			AuditID:       "fuzz",
			Action:        act,
			SubtypeCode:   sub,
			Outcome:       out,
			OutcomeDesc:   desc,
			HTTPMethod:    method,
			HTTPStatus:    status,
			RequestURI:    rawPath,
			EntityType:    eType,
			EntityID:      eID,
			EntityVersion: eVer,
		})
	})
}
