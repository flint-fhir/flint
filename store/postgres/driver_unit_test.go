package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"errors"
	"io"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/flint-fhir/flint/pkg/fhirutil"
)

type mockPGDriver struct {
	mu          sync.Mutex
	existsInDB  bool
	isDeleted   bool
	searchCount int
}

type mockPGConn struct {
	drv *mockPGDriver
}

type mockPGTx struct{}

type mockPGRows struct {
	cols []string
	rows [][]driver.Value
	idx  int
}

func (d *mockPGDriver) Open(_ string) (driver.Conn, error) {
	return &mockPGConn{drv: d}, nil
}

func (c *mockPGConn) Prepare(_ string) (driver.Stmt, error) { return nil, errors.New("unsupported") }
func (c *mockPGConn) Close() error                          { return nil }
func (c *mockPGConn) Begin() (driver.Tx, error)             { return &mockPGTx{}, nil }
func (c *mockPGConn) Ping(_ context.Context) error          { return nil }

func (c *mockPGConn) ExecContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}

func (c *mockPGConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.drv.mu.Lock()
	exists := c.drv.existsInDB
	deleted := c.drv.isDeleted
	sCount := c.drv.searchCount
	c.drv.mu.Unlock()

	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	q := strings.TrimSpace(query)

	switch {
	case strings.Contains(q, "SELECT res_version, is_deleted") && strings.Contains(q, "FROM fhir_resource"):
		if !exists {
			return &mockPGRows{cols: []string{"res_version", "is_deleted"}}, nil
		}
		return &mockPGRows{
			cols: []string{"res_version", "is_deleted"},
			rows: [][]driver.Value{{int64(1), deleted}},
		}, nil

	case strings.Contains(q, "SELECT res_version, last_updated, is_deleted") && strings.Contains(q, "FOR UPDATE"):
		if !exists {
			return &mockPGRows{cols: []string{"res_version", "last_updated", "is_deleted"}}, nil
		}
		return &mockPGRows{
			cols: []string{"res_version", "last_updated", "is_deleted"},
			rows: [][]driver.Value{{int64(1), now, deleted}},
		}, nil

	case strings.Contains(q, "RETURNING res_version, last_updated"):
		return &mockPGRows{
			cols: []string{"res_version", "last_updated"},
			rows: [][]driver.Value{{int64(2), now}},
		}, nil

	case strings.Contains(q, "SELECT resource_proto FROM fhir_resource"):
		return &mockPGRows{
			cols: []string{"resource_proto"},
			rows: [][]driver.Value{{[]byte{0x0a, 0x05, 'p', 'a', 't', '-', '1'}}},
		}, nil

	case strings.Contains(q, "SELECT resource_proto, res_version, last_updated, is_deleted"):
		return &mockPGRows{
			cols: []string{"resource_proto", "res_version", "last_updated", "is_deleted"},
			rows: [][]driver.Value{{[]byte{0x0a, 0x05, 'p', 'a', 't', '-', '1'}, int64(1), now, deleted}},
		}, nil

	case strings.Contains(q, "SELECT is_deleted FROM fhir_resource"):
		if !exists {
			return &mockPGRows{cols: []string{"is_deleted"}}, nil
		}
		return &mockPGRows{
			cols: []string{"is_deleted"},
			rows: [][]driver.Value{{deleted}},
		}, nil

	case strings.Contains(q, "SELECT EXISTS ("):
		return &mockPGRows{
			cols: []string{"exists"},
			rows: [][]driver.Value{{true}},
		}, nil

	case strings.Contains(q, "SELECT COUNT("):
		return &mockPGRows{
			cols: []string{"count"},
			rows: [][]driver.Value{{int64(sCount)}},
		}, nil

	case strings.Contains(q, "SELECT DISTINCT u.sp_value"):
		return &mockPGRows{
			cols: []string{"sp_value"},
			rows: [][]driver.Value{{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient"}},
		}, nil

	case strings.Contains(q, "SELECT DISTINCT t.sp_name, t.sp_system, t.sp_value"):
		return &mockPGRows{
			cols: []string{"sp_name", "sp_system", "sp_value"},
			rows: [][]driver.Value{
				{"_tag", "http://example.org", "vip"},
				{"_security", "http://terminology.hl7.org/CodeSystem/v3-Confidentiality", "R"},
			},
		}, nil

	case strings.Contains(q, "SELECT r.res_type, r.res_id, r.res_version") && !strings.Contains(q, "r.last_updated"):
		return &mockPGRows{
			cols: []string{"res_type", "res_id", "res_version"},
			rows: [][]driver.Value{{"Observation", "obs-1", int64(1)}},
		}, nil

	case strings.Contains(q, "SELECT res_type, res_id FROM fhir_resource"):
		return &mockPGRows{
			cols: []string{"res_type", "res_id"},
			rows: [][]driver.Value{{"Observation", "obs-1"}},
		}, nil

	case strings.Contains(q, "SELECT h.res_type, h.res_id, h.resource_proto, h.res_version, h.last_updated, h.is_deleted FROM fhir_resource_history"):
		return &mockPGRows{
			cols: []string{"res_type", "res_id", "resource_proto", "res_version", "last_updated", "is_deleted"},
			rows: [][]driver.Value{{"Patient", "pat-1", []byte{0x0a}, int64(1), now, false}},
		}, nil

	case strings.Contains(q, "SELECT res_type, res_id, resource_proto FROM fhir_resource"):
		return &mockPGRows{
			cols: []string{"res_type", "res_id", "resource_proto"},
			rows: [][]driver.Value{{"Patient", "pat-1", []byte{}}},
		}, nil

	case strings.Contains(q, "r.res_type, r.res_id, r.res_version, r.last_updated, r.resource_proto"):
		if sCount == 0 || strings.Contains(q, "r.res_type >") {
			return &mockPGRows{cols: []string{"res_type", "res_id", "res_version", "last_updated", "resource_proto"}}, nil
		}
		return &mockPGRows{
			cols: []string{"res_type", "res_id", "res_version", "last_updated", "resource_proto"},
			rows: [][]driver.Value{{"Patient", "pat-1", int64(1), now, []byte{0x0a}}},
		}, nil

	case strings.Contains(q, "FROM fhir_audit_event"):
		return &mockPGRows{
			cols: []string{
				"tenant_id", "audit_id", "recorded", "action", "subtype_code", "outcome", "outcome_desc",
				"http_method", "http_status", "request_uri", "agent_subject", "agent_patient",
				"client_ip", "entity_type", "entity_id", "entity_version",
			},
			rows: [][]driver.Value{{
				"t1", "aud-1", now, "R", "read", "0", "200 OK",
				"GET", int64(200), "/fhir/r4/t1/Patient/pat-1", "practitioner-1", "pat-1",
				"127.0.0.1", "Patient", "pat-1", "1",
			}},
		}, nil

	case strings.Contains(q, "DELETE FROM fhir_oauth_auth_code"):
		return &mockPGRows{
			cols: []string{
				"client_id", "redirect_uri", "scope", "code_challenge", "code_challenge_method",
				"patient_id", "encounter_id", "tenant_id", "nonce", "expires_at",
			},
			rows: [][]driver.Value{{
				"inferno", "https://cb", "patient/*.rs", "chal", "S256",
				"pat-1", "enc-1", "t1", "nonce-1", now.Add(time.Hour),
			}},
		}, nil

	case strings.Contains(q, "DELETE FROM fhir_oauth_refresh_token"):
		return &mockPGRows{
			cols: []string{"subject", "patient_id", "encounter_id", "scope", "expires_at"},
			rows: [][]driver.Value{{"sub-1", "pat-1", "enc-1", "patient/*.rs", now.Add(time.Hour)}},
		}, nil

	case strings.Contains(q, "SELECT 1 FROM fhir_oauth_revoked_token"):
		return &mockPGRows{
			cols: []string{"exists"},
			rows: [][]driver.Value{{int64(1)}},
		}, nil

	case strings.Contains(q, "FROM fhir_export_job"):
		b64 := base64.StdEncoding.EncodeToString([]byte(`{"resourceType":"Patient","id":"pat-1"}` + "\n"))
		return &mockPGRows{
			cols: []string{"job_id", "tenant_id", "patient_id", "request_url", "created_at", "types_order", "counts", "files_json", "combined"},
			rows: [][]driver.Value{{
				"job-1", "t1", "pat-1", "http://localhost/export", now,
				`["Patient"]`, `{"Patient":1}`, `{"Patient":"` + b64 + `"}`, []byte("combined"),
			}},
		}, nil
	}

	return &mockPGRows{cols: []string{"col"}}, nil
}

func (t *mockPGTx) Commit() error   { return nil }
func (t *mockPGTx) Rollback() error { return nil }

func (r *mockPGRows) Columns() []string { return r.cols }
func (r *mockPGRows) Close() error      { return nil }
func (r *mockPGRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.rows) {
		return io.EOF
	}
	row := r.rows[r.idx]
	r.idx++
	copy(dest, row)
	return nil
}

var (
	mockPGDrvOnce sync.Once
	sharedMockDrv = &mockPGDriver{searchCount: 1}
)

func openMockPGStore(t *testing.T, opts ...Option) *Store {
	t.Helper()
	mockPGDrvOnce.Do(func() {
		sql.Register("flint_mock_pg", sharedMockDrv)
	})
	db, err := sql.Open("flint_mock_pg", "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return New(db, opts...)
}

func TestETagFormattingAndParsingUnit(t *testing.T) {
	assert.Equal(t, `W/"3"`, FormatETag(3))

	v, err := ParseETagVersion(`W/"3"`)
	require.NoError(t, err)
	assert.Equal(t, 3, v)

	v, err = ParseETagVersion(`"5"`)
	require.NoError(t, err)
	assert.Equal(t, 5, v)

	v, err = ParseETagVersion(`7`)
	require.NoError(t, err)
	assert.Equal(t, 7, v)

	for _, bad := range []string{"", `W/3`, `"unclosed`, `W/"0"`, `W/"-2"`, `W/"abc"`} {
		_, err := ParseETagVersion(bad)
		assert.ErrorIs(t, err, ErrInvalidETag, "expected ErrInvalidETag for %q", bad)
	}
}

func TestStoreCRUDHistoryAuditOAuthAndExportOfflineUnit(t *testing.T) {
	ctx := t.Context()
	st := openMockPGStore(t, WithTenantIsolation(TenantIsolationSchema), WithTenantRoleSwitch(true))
	require.NoError(t, st.Ping(ctx))

	var nilStore *Store
	require.NoError(t, nilStore.Ping(ctx))

	// 1. WriteResourceWithMeta with all 6 search index types
	sharedMockDrv.mu.Lock()
	sharedMockDrv.existsInDB = false
	sharedMockDrv.isDeleted = false
	sharedMockDrv.searchCount = 1
	sharedMockDrv.mu.Unlock()

	now := time.Now().UTC()
	input := ResourceInput{
		TenantID:      "t1",
		ResType:       "Patient",
		ResID:         "pat-1",
		ResourceProto: []byte{0x0a, 0x05, 'p', 'a', 't', '-', '1'},
		SearchIndexes: &SearchIndexes{
			Strings:    []SpidxString{{TenantID: "t1", ResType: "Patient", ResID: "pat-1", SpName: "family", SpValue: "Smith"}},
			Tokens:     []SpidxToken{{TenantID: "t1", ResType: "Patient", ResID: "pat-1", SpName: "gender", SpValue: "female"}},
			Dates:      []SpidxDate{{TenantID: "t1", ResType: "Patient", ResID: "pat-1", SpName: "birthdate", SpLow: now, SpHigh: now}},
			References: []SpidxReference{{TenantID: "t1", ResType: "Patient", ResID: "pat-1", SpName: "organization", TargetType: "Organization", TargetID: "org-1"}},
			Quantities: []SpidxQuantity{{TenantID: "t1", ResType: "Observation", ResID: "obs-1", SpName: "value-quantity", SpValue: 72.0}},
			URIs:       []SpidxURI{{TenantID: "t1", ResType: "ValueSet", ResID: "vs-1", SpName: "url", SpValue: "http://example.org/vs"}},
		},
	}
	rec, err := st.WriteResourceWithMeta(ctx, input)
	require.NoError(t, err)
	assert.Equal(t, 2, rec.ResVersion)
	assert.True(t, rec.Created)

	// 2. WriteBatch (empty, valid, and cross-tenant error)
	require.NoError(t, st.WriteBatch(ctx, nil))
	require.NoError(t, st.WriteBatch(ctx, []ResourceInput{input}))
	require.ErrorIs(t, st.WriteBatch(ctx, []ResourceInput{
		{TenantID: "t1", ResType: "Patient", ResID: "p1"},
		{TenantID: "t2", ResType: "Patient", ResID: "p2"},
	}), ErrCrossTenantBatch)

	// 3. ReadResource, ReadResourceWithMeta, ReadResourceVersion, ListHistoryWithParams
	raw, err := st.ReadResource(ctx, "t1", "Patient", "pat-1")
	require.NoError(t, err)
	assert.NotEmpty(t, raw)

	metaRec, err := st.ReadResourceWithMeta(ctx, "t1", "Patient", "pat-1")
	require.NoError(t, err)
	assert.Equal(t, 1, metaRec.ResVersion)

	verRec, err := st.ReadResourceVersion(ctx, "t1", "Patient", "pat-1", 1)
	require.NoError(t, err)
	assert.Equal(t, 1, verRec.ResVersion)

	hist, total, err := st.ListHistoryWithParams(ctx, HistoryParams{
		TenantID: "t1",
		ResType:  "Patient",
		ResID:    "pat-1",
		Since:    &now,
		At: []DateOp{
			{Prefix: "eq", Value: now, High: now.Add(time.Hour)},
			{Prefix: "ge", Value: now},
		},
		List:  []string{"List/list-1,list-2"},
		Limit: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, hist, 1)

	// 4. Reindex
	reidxRes, err := st.Reindex(ctx, ReindexParams{
		TenantID:   "t1",
		ResType:    "Patient",
		ResID:      "pat-1",
		Extractors: DefaultIndexExtractors(),
	})
	require.NoError(t, err)
	assert.Equal(t, 1, reidxRes.ReindexedResources)

	// 5. DeleteResource & ExecuteTransactionBundle
	sharedMockDrv.mu.Lock()
	sharedMockDrv.existsInDB = true
	sharedMockDrv.searchCount = 0
	sharedMockDrv.mu.Unlock()

	require.NoError(t, st.DeleteResource(ctx, "t1", "Patient", "pat-1"))

	bundleRes, err := st.ExecuteTransactionBundle(ctx, []BundleOperation{
		{
			OpType:      BundleOpWrite,
			Method:      "POST",
			Input:       input,
			IfNoneExist: &SearchParams{TenantID: "t1", ResType: "Patient"},
		},
		{
			OpType:            BundleOpWrite,
			Method:            "PUT",
			Input:             input,
			ConditionalParams: &SearchParams{TenantID: "t1", ResType: "Patient"},
			BuildWriteInput: func(_ string, _ bool) (ResourceInput, error) {
				return input, nil
			},
		},
		{
			OpType:            BundleOpDelete,
			Method:            "DELETE",
			Input:             input,
			ConditionalParams: &SearchParams{TenantID: "t1", ResType: "Patient"},
		},
	})
	require.NoError(t, err)
	require.Len(t, bundleRes, 3)

	// 6. AuditEvents (WriteAuditEvent, ReadAuditEvent, QueryAuditEvents)
	sharedMockDrv.mu.Lock()
	sharedMockDrv.searchCount = 1
	sharedMockDrv.mu.Unlock()

	require.NoError(t, st.WriteAuditEvent(ctx, AuditRecord{
		TenantID: "t1",
		AuditID:  "aud-1",
		Action:   "R",
	}))
	audRec, err := st.ReadAuditEvent(ctx, "t1", "aud-1")
	require.NoError(t, err)
	assert.Equal(t, "aud-1", audRec.AuditID)

	audList, audTotal, err := st.QueryAuditEvents(ctx, AuditQueryParams{
		TenantID:     "t1",
		Action:       "R",
		SubtypeCode:  "read",
		Outcome:      "0",
		AgentSubject: "practitioner-1",
		AgentPatient: "pat-1",
		EntityType:   "Patient",
		EntityID:     "pat-1",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, audTotal)
	require.Len(t, audList, 1)

	// 7. OAuth2 & Bulk Export Job persistence
	require.NoError(t, st.SaveAuthCode(ctx, "code-123", OAuthAuthCodeRecord{
		ClientID:  "inferno",
		TenantID:  "t1",
		ExpiresAt: now.Add(time.Hour),
	}))
	acRec, ok, err := st.ConsumeAuthCode(ctx, "code-123")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "inferno", acRec.ClientID)

	require.NoError(t, st.SaveRefreshToken(ctx, "rt-123", OAuthRefreshTokenRecord{
		Subject:   "sub-1",
		ExpiresAt: now.Add(time.Hour),
	}))
	rtRec, ok, err := st.ConsumeRefreshToken(ctx, "rt-123")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "sub-1", rtRec.Subject)

	require.NoError(t, st.RevokeToken(ctx, "rev-123", 0))
	revoked, err := st.IsTokenRevoked(ctx, "rev-123")
	require.NoError(t, err)
	assert.True(t, revoked)

	require.NoError(t, st.SaveExportJob(ctx, ExportJobRecord{
		ID:         "job-1",
		TenantID:   "t1",
		PatientID:  "pat-1",
		CreatedAt:  now,
		TypesOrder: []string{"Patient"},
		Counts:     map[string]int{"Patient": 1},
		Files:      map[string][]byte{"Patient": []byte(`{"resourceType":"Patient","id":"pat-1"}` + "\n")},
	}))
	expJob, err := st.GetExportJob(ctx, "t1", "job-1")
	require.NoError(t, err)
	assert.Equal(t, "job-1", expJob.ID)
	assert.Contains(t, string(expJob.Files["Patient"]), "pat-1")

	deletedJob, err := st.DeleteExportJob(ctx, "t1", "job-1")
	require.NoError(t, err)
	assert.True(t, deletedJob)

	// 8. ExportResources (EmptyCohort, PatientIDs, PatientCompartment, ResourceTypes, Since, Until)
	emptyExp, err := st.ExportResources(ctx, ExportParams{TenantID: "t1", EmptyCohort: true})
	require.NoError(t, err)
	assert.Empty(t, emptyExp)

	cohortExp, err := st.ExportResources(ctx, ExportParams{
		TenantID:      "t1",
		PatientIDs:    []string{"Patient/pat-1"},
		ResourceTypes: []string{"Patient"},
		Since:         &now,
		Until:         &now,
	})
	require.NoError(t, err)
	require.Len(t, cohortExp, 1)

	compExp, err := st.ExportResources(ctx, ExportParams{
		TenantID:           "t1",
		PatientCompartment: true,
	})
	require.NoError(t, err)
	require.Len(t, compExp, 1)

	singlePatExp, err := st.ExportResources(ctx, ExportParams{
		TenantID:  "t1",
		PatientID: "pat-1",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, singlePatExp)

	// 9. Rich Search, Everything, Graph, MPI, Purge, Expunge, Meta, and Conditional CRUD
	sr, err := st.Search(ctx, SearchParams{
		TenantID:        "t1",
		ResType:         "Observation",
		ResTypes:        []string{"Observation", "Patient"},
		ID:              "Patient/pat-1,obs-1",
		CompartmentType: "Patient",
		CompartmentID:   "Patient/pat-1",
		LastUpdated: []DateOp{
			{Prefix: "ge", Value: now.Add(-time.Hour)},
			{Prefix: "ap", Value: now},
		},
		StringFilters: map[string][]string{
			"family":          {"Smith,Jones"},
			"family:exact":    {"Smith"},
			"family:contains": {"mit"},
			"code:text":       {"heart"},
		},
		TokenFilters: map[string][]string{
			"code":               {"http://loinc.org|8867-4,|8867-4,http://loinc.org|,8867-4"},
			"status":             {"final,http://hl7.org/fhir/observation-status|final"},
			"status:not":         {"cancelled"},
			"identifier:of-type": {"http://terminology.hl7.org/CodeSystem/v2-0203|MR|12345,|MR|12345"},
		},
		DateFilters: map[string][]DateOp{
			"date": {
				{Prefix: "eq", Value: now, High: now.Add(time.Hour)},
				{Prefix: "gt", Value: now},
				{Prefix: "lt", Value: now},
				{Prefix: "le", Value: now, High: now.Add(time.Hour)},
				{Prefix: "sa", Value: now, High: now.Add(time.Hour)},
				{Prefix: "ne", Value: now, High: now.Add(time.Hour)},
				{Prefix: "ap", Value: now},
			},
		},
		QuantityFilters: map[string][]QuantityOp{
			"value-quantity": {
				{Prefix: "eq", Value: 70, System: "http://unitsofmeasure.org", Code: "kg"},
				{Prefix: "ap", Value: 98.6, System: "http://unitsofmeasure.org", Code: "[degF]"},
				{Prefix: "ne", Value: 37.0, System: "http://unitsofmeasure.org", Code: "Cel"},
				{Prefix: "gt", Value: 50},
			},
		},
		ReferenceFilters: map[string][]string{
			"subject":         {"Patient/pat-1,pat-2"},
			"subject:Patient": {"pat-1"},
			"performer:not":   {"Practitioner/prac-9"},
		},
		URIFilters: map[string][]string{
			"url":       {"http://example.org/fhir/ValueSet/vs-1"},
			"url:below": {"http://example.org/fhir"},
			"url:above": {"http://example.org/fhir/ValueSet/vs-1"},
		},
		Missing: map[string]bool{
			"deceased": true,
			"value":    false,
		},
		Chained: []ChainedParam{
			{Hops: []ChainedHop{{RefParam: "subject", TargetType: "Patient"}}, TargetParam: "family", Value: "Smith", ParamType: "string"},
			{Hops: []ChainedHop{{RefParam: "subject", TargetType: "Patient"}}, TargetParam: "identifier", Value: "http://sys|123", ParamType: "token"},
			{Hops: []ChainedHop{{RefParam: "subject", TargetType: "Patient"}}, TargetParam: "birthdate", Value: "2000-01-01", ParamType: "date"},
			{Hops: []ChainedHop{{RefParam: "subject", TargetType: "Patient"}}, TargetParam: "organization", Value: "Organization/org-1", ParamType: "reference"},
		},
		Has: []HasParam{
			{SourceType: "Observation", RefParam: "patient", SearchParam: "code", Value: "http://loinc.org|8867-4", ParamType: "token"},
			{SourceType: "Observation", RefParam: "patient", SearchParam: "value-quantity", Value: "gt70", ParamType: "quantity"},
			{SourceType: "Observation", RefParam: "patient", SearchParam: "_id", Value: "obs-1", ParamType: "id"},
		},
		List:    []string{"List/list-1"},
		Content: []string{"heart rate"},
		Text:    []string{"normal sinus"},
		Filters: []FilterExpr{
			{
				Kind: "and",
				Children: []FilterExpr{
					{Kind: "param", Param: "code", Op: "eq", Value: "8867-4"},
					{Kind: "param", Param: "family", Op: "co", Value: "mit"},
					{Kind: "param", Param: "date", Op: "ge", Value: "2020-01-01"},
					{Kind: "param", Param: "value-quantity", Op: "gt", Value: "60"},
					{Kind: "param", Param: "subject", Op: "eq", Value: "Patient/pat-1"},
					{Kind: "param", Param: "url", Op: "sb", Value: "http://example.org"},
					{Kind: "param", Param: "deceased", Op: "pr", Value: "false"},
					{Kind: "not", Children: []FilterExpr{{Kind: "param", Param: "_id", Op: "ne", Value: "obs-999"}}},
				},
			},
		},
		CompositeTokenQuantities: [][]CompositeTokenQuantityParam{
			{
				{ParamName: "code-value-quantity", TokenSystem: "http://loinc.org", TokenCode: "8867-4", Quantity: QuantityOp{Prefix: "gt", Value: 60}},
				{ParamName: "component-code-value-quantity", TokenSystem: "http://loinc.org", TokenCode: "8480-6", Quantity: QuantityOp{Prefix: "le", Value: 120}},
				{ParamName: "combo-code-value-quantity", TokenSystem: "http://loinc.org", TokenCode: "8480-6", Quantity: QuantityOp{Prefix: "le", Value: 120}},
			},
		},
		CompositeTokenTokens: [][]CompositeTokenTokenParam{
			{
				{ParamName: "code-value-concept", TokenSystem: "http://loinc.org", TokenCode: "1", ValueSystem: "http://snomed.info/sct", ValueCode: "2"},
				{ParamName: "component-code-value-concept", TokenSystem: "http://loinc.org", TokenCode: "1", ValueCode: "2"},
				{ParamName: "combo-code-value-concept", TokenCode: "1", ValueCode: "2"},
			},
		},
		CompositeTokenDates: [][]CompositeTokenDateParam{
			{{ParamName: "code-value-date", TokenSystem: "http://loinc.org", TokenCode: "1", Date: DateOp{Prefix: "eq", Value: now}}},
		},
		CompositeTokenStrings: [][]CompositeTokenStringParam{
			{{ParamName: "code-value-string", TokenSystem: "http://loinc.org", TokenCode: "1", Value: "abc"}},
		},
		Sort: []SortSpec{
			{Param: "_id", Desc: false},
			{Param: "_lastUpdated", Desc: true},
			{Param: "date", Desc: true},
			{Param: "value-quantity", Desc: false},
			{Param: "family", Desc: false},
			{Param: "subject", Desc: false},
			{Param: "url", Desc: false},
			{Param: "code", Desc: false},
		},
		Includes:    []IncludeParam{{SourceType: "Observation", ParamName: "subject", TargetType: "Patient"}},
		RevIncludes: []IncludeParam{{SourceType: "Provenance", ParamName: "target", TargetType: "Observation"}},
		Count:       10,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, sr.Total)

	// PatientEverything, EncounterEverything, GroupEverything
	pev, err := st.PatientEverything(ctx, PatientEverythingParams{
		TenantID:      "t1",
		PatientID:     "pat-1",
		ResourceTypes: []string{"Patient", "Observation"},
		Since:         &now,
		Until:         &now,
		Start:         &now,
		End:           &now,
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, pev.Total, 1)

	eev, err := st.EncounterEverything(ctx, EncounterEverythingParams{
		TenantID:      "t1",
		EncounterID:   "enc-1",
		ResourceTypes: []string{"Encounter", "Observation"},
		Since:         &now,
		Until:         &now,
		Start:         &now,
		End:           &now,
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, eev.Total, 1)

	gev, err := st.GroupEverything(ctx, GroupEverythingParams{
		TenantID:      "t1",
		GroupID:       "grp-1",
		PatientIDs:    []string{"Patient/pat-1"},
		ResourceTypes: []string{"Group", "Patient"},
		Since:         &now,
		Until:         &now,
		Start:         &now,
		End:           &now,
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, gev.Total, 1)

	// MPI Candidates, ResourceGraph, AggregateMeta, Compartment checks
	cands, err := st.SearchPatientMatchCandidates(ctx, PatientMatchCandidateParams{
		TenantID:            "t1",
		RestrictedPatientID: "pat-1",
		ExtraCandidateIDs:   []string{"pat-2"},
		Identifiers:         []string{"MRN-1"},
		Families:            []string{"Smith"},
		Givens:              []string{"Alice"},
		BirthDate:           "1985-04-12",
	})
	require.NoError(t, err)
	require.Len(t, cands, 1)

	gr, err := st.ResourceGraph(ctx, ResourceGraphParams{
		TenantID:     "t1",
		ResourceType: "Patient",
		ResourceID:   "pat-1",
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, gr.Total, 1)

	agg, err := st.AggregateMeta(ctx, "t1", "Patient")
	require.NoError(t, err)
	assert.NotEmpty(t, agg.Profiles)
	assert.NotEmpty(t, agg.Tags)
	assert.NotEmpty(t, agg.Security)

	inComp, err := st.BelongsToPatientCompartment(ctx, "t1", "Observation", "obs-1", "pat-1")
	require.NoError(t, err)
	assert.True(t, inComp)
	assert.True(t, IndexesBelongToPatientCompartment("Observation", "obs-1", &SearchIndexes{
		References: []SpidxReference{{SpName: "subject", TargetType: "Patient", TargetID: "pat-1"}},
	}, "pat-1"))
	assert.False(t, IndexesBelongToPatientCompartment("Observation", "obs-1", &SearchIndexes{
		References: []SpidxReference{{SpName: "subject", TargetType: "Patient", TargetID: "other-pat"}},
	}, "pat-1"))

	// PurgePatientCompartment & ExpungeResources
	purgedSoft, err := st.PurgePatientCompartment(ctx, "t1", "pat-1", false)
	require.NoError(t, err)
	assert.NotEmpty(t, purgedSoft)

	purgedHard, err := st.PurgePatientCompartment(ctx, "t1", "pat-1", true)
	require.NoError(t, err)
	assert.NotEmpty(t, purgedHard)

	expCount, err := st.ExpungeResources(ctx, ExpungeParams{
		TenantID:                "t1",
		ResType:                 "Observation",
		ResID:                   "obs-1",
		ExpungeDeletedResources: true,
		ExpungePreviousVersions: true,
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, expCount, 1)

	// ConditionalCreateResource, ConditionalUpdateResource, ConditionalDeleteResource
	_, createdCond, err := st.ConditionalCreateResource(ctx, input, SearchParams{TenantID: "t1", ResType: "Patient"})
	require.NoError(t, err)
	assert.False(t, createdCond)

	_, err = st.ConditionalUpdateResource(ctx, "t1", "Patient", SearchParams{TenantID: "t1", ResType: "Patient"}, func(_ string, _ bool) (ResourceInput, error) {
		return input, nil
	})
	require.NoError(t, err)

	_, err = st.ConditionalDeleteResource(ctx, "t1", "Patient", SearchParams{TenantID: "t1", ResType: "Patient"}, func(_ string) error {
		return nil
	})
	require.NoError(t, err)
}

func TestWave57PostgresCoverageHardening(t *testing.T) {
	ctx := t.Context()
	st := openMockPGStore(t)
	db := st.db

	// 1. Schema migrations, tenant provisioning, isolation modes & PurgeTenant
	require.NoError(t, RunMigrations(ctx, db))
	schemaName, err := ProvisionTenantSchema(ctx, db, "t-wave57")
	require.NoError(t, err)
	assert.Equal(t, "tenant_t_wave57", schemaName)
	_, err = ProvisionTenantSchema(ctx, db, "INVALID SLUG!")
	require.Error(t, err)
	require.NoError(t, PurgeTenant(ctx, db, "t-wave57"))
	require.Error(t, PurgeTenant(ctx, db, "INVALID SLUG!"))

	for _, rawMode := range []string{"", "schema", "per-tenant", "shared", "public", "disabled"} {
		_, err := ParseTenantIsolationMode(rawMode)
		require.NoError(t, err)
	}
	_, err = ParseTenantIsolationMode("invalid-mode")
	require.Error(t, err)

	sharedStore := New(db,
		WithTenantIsolation(TenantIsolationShared),
		WithAutoProvision(true),
		WithTenantRoleSwitch(true),
	)
	assert.Equal(t, TenantIsolationShared, sharedStore.TenantIsolation())
	require.NoError(t, sharedStore.WriteResource(ctx, ResourceInput{
		TenantID:      "t-wave57",
		ResType:       "Patient",
		ResID:         "p1",
		ResourceProto: []byte{0x0a},
	}))
	sharedStore.SetTenantIsolation(TenantIsolationSchema)
	assert.Equal(t, TenantIsolationSchema, sharedStore.TenantIsolation())
	_, err = sharedStore.EnsureTenantSchema(ctx, "t-wave57")
	require.NoError(t, err)
	_, err = sharedStore.EnsureTenantSchema(ctx, "t-wave57") // cached in provisionedSchemas
	require.NoError(t, err)
	require.NoError(t, sharedStore.WriteResource(ctx, ResourceInput{
		TenantID:      "t-wave57",
		ResType:       "Patient",
		ResID:         "p1",
		ResourceProto: []byte{0x0a},
	}))

	// 2. Custom SearchParameters registration & dynamic extraction across FHIR R4 types
	customDefs := []CustomSearchParamDef{
		{ID: "sp-str", Code: "custom-str", Base: []string{"Patient"}, Type: "string", Expression: "(Patient.name.family) | Patient.name.given"},
		{ID: "sp-tok", Code: "custom-tok", Base: []string{"Patient"}, Type: "token", Expression: "Patient.extension('http://example.org/ext-code')"},
		{ID: "sp-date", Code: "custom-date", Base: []string{"Patient"}, Type: "date", Expression: "Patient.birthDate"},
		{ID: "sp-qty", Code: "custom-qty", Base: []string{"Observation"}, Type: "quantity", Expression: "Observation.valueQuantity"},
		{ID: "sp-ref", Code: "custom-ref", Base: []string{"Observation"}, Type: "reference", Expression: "Observation.subject"},
		{ID: "sp-uri", Code: "custom-uri", Base: []string{"ConceptMap"}, Type: "uri", Expression: "ConceptMap.url"},
		{ID: "sp-obs-str", Code: "custom-obs-str", Base: []string{"Observation"}, Type: "string", Expression: "Observation.code.coding | Observation.code | Observation.identifier | Observation.status"},
		{ID: "sp-ext-where", Code: "custom-where", Base: []string{"Patient"}, Type: "string", Expression: "Patient.extension.where(url='http://example.org/ext-where').valueString"},
	}
	for _, cd := range customDefs {
		RegisterCustomSearchParam(cd)
	}
	defer func() {
		for _, cd := range customDefs {
			UnregisterCustomSearchParam(cd.ID)
		}
	}()
	assert.NotEmpty(t, CustomSearchParamsForType("Patient"))

	descs := fhirutil.AllR4ResourceDescriptors()
	staticExtractors := DefaultIndexExtractors()

	testResources := []struct {
		resType string
		id      string
		rawJSON string
	}{
		{
			resType: "Patient",
			id:      "pat-dyn-1",
			rawJSON: `{
				"resourceType": "Patient",
				"id": "pat-dyn-1",
				"meta": {
					"source": "http://source.example.org",
					"profile": ["http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient"],
					"security": [{"system": "http://terminology.hl7.org/CodeSystem/v3-Confidentiality", "code": "N", "display": "Normal"}],
					"tag": [{"system": "http://example.org/tag", "code": "vip", "display": "VIP Patient"}]
				},
				"text": {"status": "generated", "div": "<div xmlns=\"http://www.w3.org/1999/xhtml\"><p>Alice Smith Narrative</p></div>"},
				"extension": [
					{
						"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-race",
						"extension": [
							{"url": "ombCategory", "valueCoding": {"system": "urn:oid:2.16.840.1.113883.6.238", "code": "2106-3", "display": "White"}},
							{"url": "text", "valueString": "White"}
						]
					},
					{
						"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-ethnicity",
						"extension": [
							{"url": "ombCategory", "valueCoding": {"system": "urn:oid:2.16.840.1.113883.6.238", "code": "2186-5", "display": "Not Hispanic or Latino"}}
						]
					},
					{
						"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-birthsex",
						"valueCode": "F"
					},
					{"url": "http://example.org/ext-code", "valueCodeableConcept": {"coding": [{"system": "http://example.org", "code": "C1"}], "text": "Concept 1"}},
					{"url": "http://example.org/ext-code", "valueIdentifier": {"system": "http://example.org/id", "value": "ID-99"}},
					{"url": "http://example.org/ext-code", "valueReference": {"reference": "Organization/org-1"}},
					{"url": "http://example.org/ext-code", "valueDate": "2020-01-01"},
					{"url": "http://example.org/ext-code", "valuePeriod": {"start": "2020-01-01", "end": "2021-01-01"}},
					{"url": "http://example.org/ext-code", "valueQuantity": {"value": 12.5, "unit": "mg", "system": "http://unitsofmeasure.org", "code": "mg"}},
					{"url": "http://example.org/ext-code", "valueDecimal": 3.14},
					{"url": "http://example.org/ext-code", "valueInteger": 42},
					{"url": "http://example.org/ext-code", "valueBoolean": true},
					{"url": "http://example.org/ext-code", "valueUri": "http://example.org/u1"},
					{"url": "http://example.org/ext-where", "valueString": "MatchedWhereValue"}
				],
				"identifier": [{
					"type": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "MR", "display": "Medical Record"}], "text": "MRN"},
					"system": "http://hospital.example.org/mrn",
					"value": "MRN-777"
				}],
				"active": true,
				"name": [{"family": "Smith", "given": ["Alice", "Marie"], "text": "Alice Marie Smith"}],
				"gender": "female",
				"birthDate": "1985-04-12",
				"deceasedBoolean": false,
				"address": [{"city": "Boston", "state": "MA", "postalCode": "02115", "country": "US"}],
				"generalPractitioner": [{"reference": "Practitioner/prac-1"}],
				"managingOrganization": {"reference": "Organization/org-1"}
			}`,
		},
		{
			resType: "MedicationRequest",
			id:      "mr-dyn-1",
			rawJSON: `{
				"resourceType": "MedicationRequest",
				"id": "mr-dyn-1",
				"contained": [{
					"resourceType": "Medication",
					"id": "med-inline-1",
					"code": {"coding": [{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "723", "display": "Amoxicillin"}], "text": "Amoxicillin"}
				}],
				"status": "active",
				"intent": "order",
				"priority": "routine",
				"medicationReference": {"reference": "#med-inline-1"},
				"subject": {"reference": "Patient/pat-dyn-1", "identifier": {"system": "http://hospital.example.org/mrn", "value": "MRN-777"}},
				"encounter": {"reference": "Encounter/enc-1"},
				"authoredOn": "2026-01-15T10:00:00Z",
				"requester": {"reference": "Practitioner/prac-1"}
			}`,
		},
		{
			resType: "Observation",
			id:      "obs-dyn-1",
			rawJSON: `{
				"resourceType": "Observation",
				"id": "obs-dyn-1",
				"identifier": [{"system": "http://example.org/obs", "value": "OBS-1"}],
				"status": "final",
				"category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs", "display": "Vital Signs"}]}],
				"code": {"coding": [{"system": "http://loinc.org", "code": "85354-9", "display": "Blood pressure"}], "text": "BP"},
				"subject": {"reference": "Patient/pat-dyn-1"},
				"effectivePeriod": {"start": "2026-01-01T00:00:00Z", "end": "2026-01-02T00:00:00Z"},
				"valueQuantity": {"value": 120, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"},
				"component": [{
					"code": {"coding": [{"system": "http://loinc.org", "code": "8480-6", "display": "Systolic"}]},
					"valueQuantity": {"value": 120, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}
				}]
			}`,
		},
		{
			resType: "Coverage",
			id:      "cov-dyn-1",
			rawJSON: `{
				"resourceType": "Coverage",
				"id": "cov-dyn-1",
				"status": "active",
				"subscriberId": "SUB-999",
				"beneficiary": {"reference": "Patient/pat-dyn-1", "identifier": {"system": "http://mrn", "value": "MRN-777"}},
				"payor": [{"reference": "Organization/org-1"}],
				"period": {"start": "2025-01-01", "end": "2026-12-31"}
			}`,
		},
		{
			resType: "ConceptMap",
			id:      "cm-dyn-1",
			rawJSON: `{
				"resourceType": "ConceptMap",
				"id": "cm-dyn-1",
				"url": "http://example.org/ConceptMap/cm-dyn-1",
				"status": "active",
				"sourceUri": "http://example.org/vs-src",
				"targetUri": "http://example.org/vs-dst"
			}`,
		},
		{
			resType: "QuestionnaireResponse",
			id:      "qr-dyn-1",
			rawJSON: `{
				"resourceType": "QuestionnaireResponse",
				"id": "qr-dyn-1",
				"questionnaire": "http://example.org/Questionnaire/q-1",
				"status": "completed",
				"subject": {"reference": "Patient/pat-dyn-1"},
				"authored": "2026-01-15T10:00:00Z"
			}`,
		},
		{
			resType: "Goal",
			id:      "goal-dyn-1",
			rawJSON: `{
				"resourceType": "Goal",
				"id": "goal-dyn-1",
				"lifecycleStatus": "active",
				"description": {"coding": [{"system": "http://snomed.info/sct", "code": "289169006", "display": "Weight loss"}], "text": "Lose weight"},
				"subject": {"reference": "Patient/pat-dyn-1"},
				"target": [{"dueDate": "2026-06-01"}]
			}`,
		},
		{
			resType: "Group",
			id:      "grp-dyn-1",
			rawJSON: `{
				"resourceType": "Group",
				"id": "grp-dyn-1",
				"type": "person",
				"actual": true,
				"member": [{"entity": {"reference": "Patient/pat-dyn-1"}}]
			}`,
		},
		{
			resType: "Device",
			id:      "dev-dyn-1",
			rawJSON: `{
				"resourceType": "Device",
				"id": "dev-dyn-1",
				"status": "active",
				"udiCarrier": [{"deviceIdentifier": "UDI-12345"}],
				"patient": {"reference": "Patient/pat-dyn-1"}
			}`,
		},
		{
			resType: "MedicationDispense",
			id:      "md-dyn-1",
			rawJSON: `{
				"resourceType": "MedicationDispense",
				"id": "md-dyn-1",
				"status": "completed",
				"medicationCodeableConcept": {"coding": [{"code": "723"}]},
				"subject": {"reference": "Patient/pat-dyn-1"},
				"whenPrepared": "2026-01-01T09:00:00Z",
				"whenHandedOver": "2026-01-01T10:00:00Z"
			}`,
		},
	}

	for _, tr := range testResources {
		md := descs[tr.resType]
		require.NotNil(t, md)
		norm, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, []byte(tr.rawJSON))
		require.NoError(t, err, "NormalizeFHIRJSONToProtoJSON(%s)", tr.resType)
		msg := dynamicpb.NewMessage(md)
		require.NoError(t, (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(norm, msg), "Unmarshal(%s)", tr.resType)
		pBytes, err := proto.Marshal(msg)
		require.NoError(t, err)

		dynExt := NewDynamicResourceExtractor(tr.resType, md)
		idx, err := dynExt("t1", "", pBytes)
		require.NoError(t, err)
		assert.NotNil(t, idx)

		if staticExt, ok := staticExtractors[tr.resType]; ok {
			sIdx, err := staticExt("t1", tr.id, pBytes)
			require.NoError(t, err)
			assert.NotNil(t, sIdx)
		}
	}

	// 3. ParseFilterExpr, IsKnownSearchParam, ValidateSearchQuery, BuildSearchParamsFromQuery, ParseIfNoneExist, HasSearchCriteria
	validFilters := []string{
		`name eq "smith" or (gender eq 'female' and not active eq "false")`,
		`name co "sm\"ith"`,
		`deceased pr true`,
		`deceased pr false`,
		`birthdate ge 1980-01-01`,
		`value-quantity gt 5.5`,
		`subject.name eq "smith"`,
	}
	for _, vf := range validFilters {
		_, err := ParseFilterExpr(vf)
		require.NoError(t, err, "filter: %s", vf)
	}

	invalidFilters := []string{
		``,
		`   `,
		`name eq "unterminated`,
		`(name eq "smith"`,
		`name`,
		`"quotedparam" eq "x"`,
		`unknown_param_xyz eq "x"`,
		`name invalid_op "x"`,
		`name eq ""`,
		`name eq (`,
		`deceased pr maybe`,
		`birthdate eq not-a-date`,
		`value-quantity gt not-a-qty`,
		`name eq "smith" extra_token`,
	}
	for _, inf := range invalidFilters {
		_, err := ParseFilterExpr(inf)
		require.Error(t, err, "expected error for filter: %s", inf)
	}

	assert.True(t, IsKnownSearchParam("_has:Observation:patient:code"))
	assert.True(t, IsKnownSearchParam("_profile"))
	assert.True(t, IsKnownSearchParam("subject.name"))
	assert.True(t, IsKnownSearchParam("subject:Patient"))
	assert.False(t, IsKnownSearchParam("subject:invalidMod"))
	assert.False(t, IsKnownSearchParam("not.a.valid.chain.at.all"))

	validQ, _ := url.ParseQuery("_count=10&_offset=0&_list=list1&_include=Observation:patient&_include:iterate=Patient:organization&_revinclude=Provenance:target&_revinclude:iterate=Observation:subject&_sort=-_lastUpdated,name&_summary=true&_total=accurate&_lastUpdated=ge2020-01-01&_filter=name+eq+%22smith%22&_content=heart&_text=sinus&_has:Observation:patient:code=8867-4&deceased:missing=false&identifier:of-type=http://sys|MR|123&subject.birthdate=2000-01-01&subject.value-quantity=gt5&code-value-quantity=http://loinc.org|8867-4$gt60&code-value-concept=http://loinc.org|1$http://snomed.info/sct|2&code-value-date=http://loinc.org|1$2020-01-01&code-value-string=http://loinc.org|1$abc&url:below=http://example.org&birthdate=2020-01-01&value-quantity=70||kg&_type=Patient,Observation&_elements=id,name&_id=Patient/p1,p2")
	require.NoError(t, ValidateSearchQuery(validQ))
	built := BuildSearchParamsFromQuery("t1", "Observation", validQ)
	assert.True(t, HasSearchCriteria(built))

	for _, badRaw := range []string{
		"_count=0",
		"_offset=-1",
		"_list=,",
		"_include=BadInclude",
		"_revinclude=BadRevInclude",
		"_sort=name:bad",
		"_sort=-",
		"_summary=invalid",
		"_total=invalid",
		"_lastUpdated=bad-date",
		"_filter=bad",
		"_content=   ",
		"_text=   ",
		"_has:BadHas=1",
		"deceased:missing=maybe",
		"identifier:of-type=bad",
		"subject.identifier:of-type=bad",
		"subject.birthdate=bad-date",
		"subject.value-quantity=bad-qty",
		"code-value-quantity=bad",
		"code-value-concept=bad",
		"code-value-date=bad",
		"code-value-string=bad",
		"birthdate=bad-date",
		"value-quantity=bad-qty",
	} {
		bq, _ := url.ParseQuery(badRaw)
		require.Error(t, ValidateSearchQuery(bq), "expected error for query: %s", badRaw)
	}

	// ParseIfNoneExist valid & invalid cases
	_, err = ParseIfNoneExist("t1", "Patient", "Patient?identifier=http://mrn|123")
	require.NoError(t, err)
	_, err = ParseIfNoneExist("t1", "Patient", "birthdate=ge2020-01-01&birthdate=le2025-01-01")
	require.NoError(t, err)

	for _, badINE := range []string{
		"",
		"Observation?identifier=123",
		"?",
		"_include=Patient:organization",
		"identifier=",
		"name=a&name=b",
		"_id=a,b",
		"birthdate=bad-date",
	} {
		_, err := ParseIfNoneExist("t1", "Patient", badINE)
		require.Error(t, err, "expected error for If-None-Exist: %s", badINE)
	}

	assert.False(t, HasSearchCriteria(SearchParams{}))
	assert.True(t, HasSearchCriteria(SearchParams{CompartmentType: "Patient", CompartmentID: "p1"}))
	assert.True(t, HasSearchCriteria(SearchParams{Text: []string{"abc"}}))
}
