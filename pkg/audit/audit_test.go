package audit_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/pkg/audit"
	"github.com/flint-fhir/flint/store/postgres"
)

func TestClassifyHTTPInteraction(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		parts       []string
		status      int
		wantAction  string
		wantSubtype string
		wantType    string
		wantID      string
		wantVer     string
	}{
		{
			name:        "GET read",
			method:      http.MethodGet,
			parts:       []string{"fhir", "r4", "tenant-a", "Patient", "pat-1"},
			status:      http.StatusOK,
			wantAction:  audit.ActionRead,
			wantSubtype: "read",
			wantType:    "Patient",
			wantID:      "pat-1",
		},
		{
			name:        "GET vread",
			method:      http.MethodGet,
			parts:       []string{"fhir", "r4", "tenant-a", "Patient", "pat-1", "_history", "2"},
			status:      http.StatusOK,
			wantAction:  audit.ActionRead,
			wantSubtype: "vread",
			wantType:    "Patient",
			wantID:      "pat-1",
			wantVer:     "2",
		},
		{
			name:        "GET history-instance",
			method:      http.MethodGet,
			parts:       []string{"fhir", "r4", "tenant-a", "Patient", "pat-1", "_history"},
			status:      http.StatusOK,
			wantAction:  audit.ActionRead,
			wantSubtype: "history-instance",
			wantType:    "Patient",
			wantID:      "pat-1",
		},
		{
			name:        "GET search-type",
			method:      http.MethodGet,
			parts:       []string{"fhir", "r4", "tenant-a", "Observation"},
			status:      http.StatusOK,
			wantAction:  audit.ActionExecute,
			wantSubtype: "search-type",
			wantType:    "Observation",
		},
		{
			name:        "POST create",
			method:      http.MethodPost,
			parts:       []string{"fhir", "r4", "tenant-a", "Condition"},
			status:      http.StatusCreated,
			wantAction:  audit.ActionCreate,
			wantSubtype: "create",
			wantType:    "Condition",
		},
		{
			name:        "PUT create-on-update (201)",
			method:      http.MethodPut,
			parts:       []string{"fhir", "r4", "tenant-a", "Patient", "pat-1"},
			status:      http.StatusCreated,
			wantAction:  audit.ActionCreate,
			wantSubtype: "update",
			wantType:    "Patient",
			wantID:      "pat-1",
		},
		{
			name:        "PUT update (200)",
			method:      http.MethodPut,
			parts:       []string{"fhir", "r4", "tenant-a", "Patient", "pat-1"},
			status:      http.StatusOK,
			wantAction:  audit.ActionUpdate,
			wantSubtype: "update",
			wantType:    "Patient",
			wantID:      "pat-1",
		},
		{
			name:        "DELETE resource",
			method:      http.MethodDelete,
			parts:       []string{"fhir", "r4", "tenant-a", "Encounter", "enc-9"},
			status:      http.StatusNoContent,
			wantAction:  audit.ActionDelete,
			wantSubtype: "delete",
			wantType:    "Encounter",
			wantID:      "enc-9",
		},
		{
			name:        "POST $validate",
			method:      http.MethodPost,
			parts:       []string{"fhir", "r4", "tenant-a", "Patient", "$validate"},
			status:      http.StatusOK,
			wantAction:  audit.ActionExecute,
			wantSubtype: "validate",
			wantType:    "Patient",
		},
		{
			name:        "POST Bundle",
			method:      http.MethodPost,
			parts:       []string{"fhir", "r4", "tenant-a"},
			status:      http.StatusOK,
			wantAction:  audit.ActionExecute,
			wantSubtype: "batch",
			wantType:    "Bundle",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			act, sub, eType, eID, eVer := audit.ClassifyHTTPInteraction(tc.method, tc.parts, tc.status)
			assert.Equal(t, tc.wantAction, act)
			assert.Equal(t, tc.wantSubtype, sub)
			assert.Equal(t, tc.wantType, eType)
			assert.Equal(t, tc.wantID, eID)
			assert.Equal(t, tc.wantVer, eVer)
		})
	}
}

func TestMemoryRecorderAndToFHIRResource(t *testing.T) {
	ctx := context.Background()
	rec := audit.NewMemoryRecorder()

	now := time.Date(2026, 10, 9, 20, 0, 0, 0, time.UTC)
	err := rec.Record(ctx, postgres.AuditRecord{
		TenantID:      "tenant-1",
		AuditID:       "aud-1",
		Recorded:      now,
		Action:        audit.ActionRead,
		SubtypeCode:   "read",
		Outcome:       audit.OutcomeSuccess,
		OutcomeDesc:   "200 OK",
		HTTPMethod:    http.MethodGet,
		HTTPStatus:    http.StatusOK,
		RequestURI:    "/fhir/r4/tenant-1/Patient/pat-100",
		AgentSubject:  "practitioner-42",
		AgentPatient:  "pat-100",
		ClientIP:      "127.0.0.1",
		EntityType:    "Patient",
		EntityID:      "pat-100",
		EntityVersion: "2",
	})
	require.NoError(t, err)

	got, err := rec.Read(ctx, "tenant-1", "aud-1")
	require.NoError(t, err)
	assert.Equal(t, "practitioner-42", got.AgentSubject)

	list, total, err := rec.Query(ctx, postgres.AuditQueryParams{
		TenantID:   "tenant-1",
		EntityType: "Patient",
		EntityID:   "pat-100",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, list, 1)

	fhirMap := audit.ToFHIRResource(list[0])
	assert.Equal(t, "AuditEvent", fhirMap["resourceType"])
	assert.Equal(t, "aud-1", fhirMap["id"])
	assert.Equal(t, "R", fhirMap["action"])
	assert.Equal(t, "0", fhirMap["outcome"])
}
