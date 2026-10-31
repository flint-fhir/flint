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
			name:        "GET Patient/$everything",
			method:      http.MethodGet,
			parts:       []string{"fhir", "r4", "tenant-a", "Patient", "pat-1", "$everything"},
			status:      http.StatusOK,
			wantAction:  audit.ActionExecute,
			wantSubtype: "everything",
			wantType:    "Patient",
			wantID:      "pat-1",
		},
		{
			name:        "POST conditional create 200 OK (If-None-Exist match)",
			method:      http.MethodPost,
			parts:       []string{"fhir", "r4", "tenant-a", "Patient"},
			status:      http.StatusOK,
			wantAction:  audit.ActionCreate,
			wantSubtype: "create",
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

func TestClassifyAllInteractionsOutcomesAndMemoryQueryWave55(t *testing.T) {
	cases := []struct {
		method      string
		parts       []string
		status      int
		wantAction  string
		wantSubtype string
		wantType    string
		wantID      string
	}{
		{http.MethodGet, []string{"fhir", "r4", "t1"}, 200, audit.ActionExecute, "search-system", "System", ""},
		{http.MethodGet, []string{"fhir", "r4"}, 200, audit.ActionExecute, "operation", "", ""},
		{http.MethodPost, []string{"fhir", "r4", "t1", "_search"}, 200, audit.ActionExecute, "search-system", "System", ""},
		{http.MethodPost, []string{"fhir", "r4", "t1", "$validate"}, 200, audit.ActionExecute, "validate", "Resource", ""},
		{http.MethodGet, []string{"fhir", "r4", "t1", "$export"}, 202, audit.ActionExecute, "export", "System", ""},
		{http.MethodGet, []string{"fhir", "r4", "t1", "$export-poll", "job-1"}, 200, audit.ActionRead, "export-poll", "ExportJob", "job-1"},
		{http.MethodDelete, []string{"fhir", "r4", "t1", "$export-poll", "job-1"}, 202, audit.ActionDelete, "export-cancel", "ExportJob", "job-1"},
		{http.MethodGet, []string{"fhir", "r4", "t1", "$export-file", "job-1", "Patient.ndjson"}, 200, audit.ActionRead, "export-file", "Patient", "job-1"},
		{http.MethodGet, []string{"fhir", "r4", "t1", "metadata"}, 200, audit.ActionRead, "capabilities", "CapabilityStatement", ""},
		{http.MethodGet, []string{"fhir", "r4", "t1", "Patient", "$match"}, 200, audit.ActionExecute, "match", "Patient", ""},
		{http.MethodPost, []string{"fhir", "r4", "t1", "Patient", "pat-1", "$purge"}, 200, audit.ActionExecute, "purge", "Patient", "pat-1"},
		{http.MethodPatch, []string{"fhir", "r4", "t1", "Patient", "pat-1"}, 200, audit.ActionUpdate, "patch", "Patient", "pat-1"},
		{http.MethodOptions, []string{"fhir", "r4", "t1", "Patient"}, 200, audit.ActionExecute, "operation", "Patient", ""},
	}
	for _, tc := range cases {
		act, sub, eType, eID, _ := audit.ClassifyHTTPInteraction(tc.method, tc.parts, tc.status)
		assert.Equal(t, tc.wantAction, act)
		assert.Equal(t, tc.wantSubtype, sub)
		assert.Equal(t, tc.wantType, eType)
		assert.Equal(t, tc.wantID, eID)
	}

	out0, desc0 := audit.ClassifyHTTPOutcome(0)
	assert.Equal(t, audit.OutcomeSuccess, out0)
	assert.Equal(t, "200 OK", desc0)

	out4, _ := audit.ClassifyHTTPOutcome(http.StatusNotFound)
	assert.Equal(t, audit.OutcomeMinorFailure, out4)

	out8, desc8 := audit.ClassifyHTTPOutcome(599)
	assert.Equal(t, audit.OutcomeSeriousFailure, out8)
	assert.Equal(t, "HTTP 599", desc8)

	// MemoryRecorder defaults, not-found, and filter branches
	mem := audit.NewMemoryRecorder()
	require.NoError(t, mem.Record(t.Context(), postgres.AuditRecord{
		TenantID:     "t1",
		AuditID:      "a1",
		Action:       audit.ActionCreate,
		SubtypeCode:  "create",
		Outcome:      audit.OutcomeSuccess,
		AgentSubject: "", // defaults to anonymous
		AgentPatient: "pat-1",
		EntityType:   "Observation",
		EntityID:     "obs-1",
	}))
	_, err := mem.Read(t.Context(), "t1", "missing")
	require.Error(t, err)

	recs, total, err := mem.Query(t.Context(), postgres.AuditQueryParams{
		TenantID:     "t1",
		Action:       audit.ActionCreate,
		SubtypeCode:  "create",
		Outcome:      audit.OutcomeSuccess,
		AgentSubject: "anonymous",
		AgentPatient: "pat-1",
		EntityType:   "Observation",
		EntityID:     "obs-1",
		Count:        10,
		Offset:       0,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, recs, 1)

	// Offset beyond total
	emptyRecs, total, err := mem.Query(t.Context(), postgres.AuditQueryParams{TenantID: "t1", Offset: 5})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	assert.Empty(t, emptyRecs)

	// StoreRecorder constructor check
	sr := audit.NewStoreRecorder(nil)
	require.NotNil(t, sr)
}
