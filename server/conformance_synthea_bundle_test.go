package server_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha512"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/doris"
	"github.com/flint-fhir/flint/store/postgres"
	"github.com/flint-fhir/flint/store/sqlite"
)

type syntheaTopologySnapshot struct {
	TxEntriesCreated          int
	PatientFamilyGenderCount  int
	VitalSignsCount           int
	ComboSystolicGT100Count   int
	LabHbA1cCount             int
	ActiveConditionsCount     int
	DocRefWithRevIncludeCount int
	ObsWithIncludePatCount    int
	EverythingTotal           int
	DocRefOpEntryCount        int
	ValidateHTTPStatus        int
	ExportResourceCount       int
	ExportResourceTypes       []string
	PatientDemographicsRows   []map[string]any
	ObservationVitalsRows     []map[string]any
	ConditionProblemsRows     []map[string]any
}

// TestSyntheaUSCoreBundleParity_AllFiveTopologies ingests a realistic Synthea R4 US Core
// transaction Bundle (16 clinical resources across Organization, Location, Practitioner,
// Patient, Encounter, Condition, Blood Pressure + HbA1c Observations, MedicationRequest,
// Immunization, AllergyIntolerance, Procedure, CarePlan, DiagnosticReport, DocumentReference,
// and Provenance connected via urn:uuid: internal references) across all 5 runtime store
// topologies (postgres, file-backed modernc.org/sqlite WAL with restart reload, doris,
// hybrid-postgres+doris, and hybrid-postgres+sqlite) and asserts 100% behavioral parity.
func TestSyntheaUSCoreBundleParity_AllFiveTopologies(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	type topologyFactory struct {
		name  string
		setup func(t *testing.T) (srv *server.Server, sqlitePath string, cleanup func())
	}

	topologies := []topologyFactory{
		{
			name: "postgres",
			setup: func(t *testing.T) (*server.Server, string, func()) {
				db := openMemPostgres(t)
				pg := postgres.New(db, postgres.WithTenantIsolation(postgres.TenantIsolationShared))
				srv := server.New(pg, logger)
				server.RegisterDefaultResourceTypes(srv)
				return srv, "", func() { _ = db.Close() }
			},
		},
		{
			name: "sqlite",
			setup: func(t *testing.T) (*server.Server, string, func()) {
				dbPath := filepath.Join(t.TempDir(), "synthea-wal.db")
				sqDB, err := sql.Open("sqlite", dbPath)
				require.NoError(t, err)
				sqDB.SetMaxOpenConns(1)
				require.NoError(t, sqlite.RunMigrations(ctx, sqDB, sqlite.JournalWAL))
				ss := sqlite.New(sqDB, sqlite.WithJournalMode(sqlite.JournalWAL), sqlite.WithDSN(dbPath))
				require.NoError(t, ss.LoadFromDB(ctx))
				srv := server.New(ss, logger)
				srv.SetAnalyticalStore(ss)
				server.RegisterDefaultResourceTypes(srv)
				return srv, dbPath, func() { _ = sqDB.Close() }
			},
		},
		{
			name: "doris",
			setup: func(t *testing.T) (*server.Server, string, func()) {
				ds := doris.New(nil)
				srv := server.New(ds, logger)
				srv.SetAnalyticalStore(ds)
				server.RegisterDefaultResourceTypes(srv)
				return srv, "", func() {}
			},
		},
		{
			name: "hybrid-postgres+doris",
			setup: func(t *testing.T) (*server.Server, string, func()) {
				db := openMemPostgres(t)
				pg := postgres.New(db, postgres.WithTenantIsolation(postgres.TenantIsolationShared))
				ds := doris.New(nil)
				srv := server.New(pg, logger)
				srv.SetAnalyticalStore(ds)
				server.RegisterDefaultResourceTypes(srv)
				return srv, "", func() { _ = db.Close() }
			},
		},
		{
			name: "hybrid-postgres+sqlite",
			setup: func(t *testing.T) (*server.Server, string, func()) {
				db := openMemPostgres(t)
				pg := postgres.New(db, postgres.WithTenantIsolation(postgres.TenantIsolationShared))
				dbPath := filepath.Join(t.TempDir(), "synthea-hybrid-olap.db")
				sqDB, err := sql.Open("sqlite", dbPath)
				require.NoError(t, err)
				sqDB.SetMaxOpenConns(1)
				require.NoError(t, sqlite.RunMigrations(ctx, sqDB, sqlite.JournalWAL))
				ss := sqlite.New(sqDB, sqlite.WithJournalMode(sqlite.JournalWAL), sqlite.WithDSN(dbPath))
				require.NoError(t, ss.LoadFromDB(ctx))
				srv := server.New(pg, logger)
				srv.SetAnalyticalStore(ss)
				server.RegisterDefaultResourceTypes(srv)
				return srv, "", func() {
					_ = sqDB.Close()
					_ = db.Close()
				}
			},
		},
	}

	bundleJSON := buildSyntheaUSCoreTransactionBundleJSON(t)
	var baseline *syntheaTopologySnapshot

	for idx, topo := range topologies {
		topo := topo
		tenant := "synthea"
		switch idx {
		case 0:
			tenant = "synthea-pg"
		case 1:
			tenant = "synthea-sqlite"
		case 2:
			tenant = "synthea-doris"
		case 3:
			tenant = "synthea-hyb-doris"
		case 4:
			tenant = "synthea-hyb-sqlite"
		}
		t.Run(topo.name, func(t *testing.T) {
			srv, sqlitePath, cleanup := topo.setup(t)
			defer cleanup()

			snap, assignedPatID := runSyntheaBundleTopologySuite(t, srv, tenant, bundleJSON)

			if baseline == nil {
				cp := snap
				baseline = &cp
			} else {
				assert.Equal(t, *baseline, snap, "topology %s diverged from postgres baseline", topo.name)
			}

			// For file-backed SQLite WAL, close DB, reopen from disk, LoadFromDB, and verify durable state!
			if sqlitePath != "" {
				cleanup()
				reopenedDB, err := sql.Open("sqlite", sqlitePath)
				require.NoError(t, err)
				defer reopenedDB.Close()
				reopenedDB.SetMaxOpenConns(1)

				reloadedStore := sqlite.New(reopenedDB, sqlite.WithJournalMode(sqlite.JournalWAL), sqlite.WithDSN(sqlitePath))
				require.NoError(t, reloadedStore.LoadFromDB(ctx))
				reloadedSrv := server.New(reloadedStore, logger)
				reloadedSrv.SetAnalyticalStore(reloadedStore)
				server.RegisterDefaultResourceTypes(reloadedSrv)

				tsReload := httptest.NewServer(reloadedSrv.Handler())
				defer tsReload.Close()

				resp, body := mustSyntheaHTTP(t, tsReload.Client(), http.MethodGet, tsReload.URL+"/fhir/r4/"+tenant+"/Patient/"+assignedPatID, "", nil, nil)
				require.Equal(t, http.StatusOK, resp.StatusCode, "reloaded SQLite WAL Patient read failed: %s", string(body))
				assert.Contains(t, string(body), `"SyntheaParity"`)

				respEv, bodyEv := mustSyntheaHTTP(t, tsReload.Client(), http.MethodGet, tsReload.URL+"/fhir/r4/"+tenant+"/Patient/"+assignedPatID+"/$everything", "", nil, nil)
				require.Equal(t, http.StatusOK, respEv.StatusCode)
				var evMap map[string]any
				require.NoError(t, json.Unmarshal(bodyEv, &evMap))
				assert.Equal(t, float64(snap.EverythingTotal), evMap["total"], "reloaded SQLite WAL $everything count mismatch")
			}
		})
	}
}

func runSyntheaBundleTopologySuite(t *testing.T, srv *server.Server, tenant string, bundleJSON []byte) (syntheaTopologySnapshot, string) {
	t.Helper()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := ts.Client()
	base := ts.URL + "/fhir/r4/" + tenant

	// 1. Atomic Synthea Transaction Bundle ingest with urn:uuid: resolution
	respTx, bodyTx := mustSyntheaHTTP(t, client, http.MethodPost, base, "application/fhir+json", bundleJSON, map[string]string{
		"Prefer": "return=representation",
	})
	require.Equal(t, http.StatusOK, respTx.StatusCode, "POST Synthea transaction failed: %s", string(bodyTx))

	var txBundle struct {
		ResourceType string `json:"resourceType"`
		Type         string `json:"type"`
		Entry        []struct {
			FullURL  string         `json:"fullUrl"`
			Resource map[string]any `json:"resource"`
			Response struct {
				Status   string `json:"status"`
				Location string `json:"location"`
			} `json:"response"`
		} `json:"entry"`
	}
	require.NoError(t, json.Unmarshal(bodyTx, &txBundle))
	require.Equal(t, "transaction-response", txBundle.Type)
	require.Len(t, txBundle.Entry, 16)

	idsByType := make(map[string][]string)
	for _, e := range txBundle.Entry {
		assert.True(t, strings.HasPrefix(e.Response.Status, "201"), "expected 201 Created, got %s", e.Response.Status)
		rt, _ := e.Resource["resourceType"].(string)
		id, _ := e.Resource["id"].(string)
		if rt == "" || id == "" {
			locParts := strings.Split(strings.Trim(e.Response.Location, "/"), "/")
			if len(locParts) >= 2 {
				rt = locParts[0]
				id = locParts[1]
			} else if fuParts := strings.Split(strings.Trim(e.FullURL, "/"), "/"); len(fuParts) >= 2 {
				rt = fuParts[len(fuParts)-2]
				id = fuParts[len(fuParts)-1]
			}
		}
		require.NotEmpty(t, rt, "missing resourceType in entry: %+v", e)
		require.NotEmpty(t, id, "missing id in entry: %+v", e)
		idsByType[rt] = append(idsByType[rt], id)
		if len(e.Resource) > 0 {
			rawEntry, _ := json.Marshal(e.Resource)
			assert.NotContains(t, string(rawEntry), "urn:uuid:", "unresolved urn:uuid: in %s/%s", rt, id)
		}
	}

	patID := idsByType["Patient"][0]
	bpObsID := idsByType["Observation"][0]

	// 2. Direct instance reads & rewritten reference verification
	respPat, bodyPat := mustSyntheaHTTP(t, client, http.MethodGet, base+"/Patient/"+patID, "", nil, nil)
	require.Equal(t, http.StatusOK, respPat.StatusCode)
	assert.Contains(t, string(bodyPat), `"SyntheaParity"`)

	respBP, bodyBP := mustSyntheaHTTP(t, client, http.MethodGet, base+"/Observation/"+bpObsID, "", nil, nil)
	require.Equal(t, http.StatusOK, respBP.StatusCode)
	assert.Contains(t, string(bodyBP), `"Patient/`+patID+`"`)
	assert.Contains(t, string(bodyBP), `"Encounter/`+idsByType["Encounter"][0]+`"`)

	// 3. US Core v6.1.0 searches (string, token, composite quantity, _include, _revinclude)
	patSearchCnt := countBundleEntries(t, client, base+"/Patient?family=SyntheaParity&gender=female")
	vitalCnt := countBundleEntries(t, client, base+"/Observation?patient=Patient/"+patID+"&category=vital-signs")
	comboCnt := countBundleEntries(t, client, base+"/Observation?patient=Patient/"+patID+"&component-code-value-quantity=http://loinc.org|8480-6$gt100")
	labCnt := countBundleEntries(t, client, base+"/Observation?patient=Patient/"+patID+"&code=http://loinc.org|4548-4")
	condCnt := countBundleEntries(t, client, base+"/Condition?patient=Patient/"+patID+"&clinical-status=active")
	docRevIncCnt := countBundleEntries(t, client, base+"/DocumentReference?patient=Patient/"+patID+"&_revinclude=Provenance:target")
	obsIncPatCnt := countBundleEntries(t, client, base+"/Observation?patient=Patient/"+patID+"&code=85354-9&_include=Observation:patient")

	assert.Equal(t, 1, patSearchCnt)
	assert.Equal(t, 1, vitalCnt)
	assert.Equal(t, 1, comboCnt)
	assert.Equal(t, 1, labCnt)
	assert.Equal(t, 1, condCnt)
	assert.Equal(t, 2, docRevIncCnt) // DocumentReference + Provenance
	assert.Equal(t, 2, obsIncPatCnt) // Blood Pressure Observation + Patient

	// 4. Clinical operations: Patient/$everything, DocumentReference/$docref, Patient/$validate
	respEv, bodyEv := mustSyntheaHTTP(t, client, http.MethodGet, base+"/Patient/"+patID+"/$everything", "", nil, nil)
	require.Equal(t, http.StatusOK, respEv.StatusCode)
	var evBundle map[string]any
	require.NoError(t, json.Unmarshal(bodyEv, &evBundle))
	evTotal := int(evBundle["total"].(float64))
	assert.GreaterOrEqual(t, evTotal, 13)

	docrefCnt := countBundleEntries(t, client, base+"/DocumentReference/$docref?patient="+patID)
	assert.GreaterOrEqual(t, docrefCnt, 1)

	respVal, _ := mustSyntheaHTTP(t, client, http.MethodGet, base+"/Patient/"+patID+"/$validate?profile=http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient", "", nil, nil)
	assert.Equal(t, http.StatusOK, respVal.StatusCode)

	// 5. Async Bulk Data $export (GET /Patient/$export) + NDJSON manifest verification
	respExp, bodyExp := mustSyntheaHTTP(t, client, http.MethodGet, base+"/Patient/$export", "", nil, map[string]string{
		"Prefer": "respond-async",
	})
	require.Equal(t, http.StatusAccepted, respExp.StatusCode, "Patient/$export kick-off failed: %s", string(bodyExp))
	pollURL := respExp.Header.Get("Content-Location")
	require.NotEmpty(t, pollURL)

	var manifest struct {
		Output []struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"output"`
	}
	var pollBody []byte
	for attempt := 0; attempt < 20; attempt++ {
		respPoll, bPoll := mustSyntheaHTTP(t, client, http.MethodGet, pollURL, "", nil, nil)
		if respPoll.StatusCode == http.StatusOK {
			pollBody = bPoll
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	require.NotEmpty(t, pollBody, "timed out polling Bulk Data export %s", pollURL)
	require.NoError(t, json.Unmarshal(pollBody, &manifest))

	exportedTotal := 0
	exportedTypesSet := make(map[string]bool)
	for _, outItem := range manifest.Output {
		exportedTypesSet[outItem.Type] = true
		respDL, bDL := mustSyntheaHTTP(t, client, http.MethodGet, outItem.URL, "", nil, nil)
		require.Equal(t, http.StatusOK, respDL.StatusCode)
		sc := bufio.NewScanner(bytes.NewReader(bDL))
		for sc.Scan() {
			if len(bytes.TrimSpace(sc.Bytes())) > 0 {
				exportedTotal++
			}
		}
	}
	exportedTypes := make([]string, 0, len(exportedTypesSet))
	for rt := range exportedTypesSet {
		exportedTypes = append(exportedTypes, rt)
	}
	sort.Strings(exportedTypes)
	assert.GreaterOrEqual(t, exportedTotal, 12)

	// 6. SQL-on-FHIR v2 built-in ViewDefinition/$run (exercises OLAP analytical pushdown on SQLite/Doris/Hybrid)
	patRows := runBuiltInViewNormalized(t, client, base, "patient-demographics", []string{"family", "given", "gender", "birth_date", "city", "state"})
	obsRows := runBuiltInViewNormalized(t, client, base, "observation-vitals", []string{"code", "status", "value", "unit"})
	condRows := runBuiltInViewNormalized(t, client, base, "condition-problems", []string{"code", "clinical_status", "verification_status"})

	require.Len(t, patRows, 1)
	require.Len(t, obsRows, 2)
	require.Len(t, condRows, 1)

	return syntheaTopologySnapshot{
		TxEntriesCreated:          len(txBundle.Entry),
		PatientFamilyGenderCount:  patSearchCnt,
		VitalSignsCount:           vitalCnt,
		ComboSystolicGT100Count:   comboCnt,
		LabHbA1cCount:             labCnt,
		ActiveConditionsCount:     condCnt,
		DocRefWithRevIncludeCount: docRevIncCnt,
		ObsWithIncludePatCount:    obsIncPatCnt,
		EverythingTotal:           evTotal,
		DocRefOpEntryCount:        docrefCnt,
		ValidateHTTPStatus:        respVal.StatusCode,
		ExportResourceCount:       exportedTotal,
		ExportResourceTypes:       exportedTypes,
		PatientDemographicsRows:   patRows,
		ObservationVitalsRows:     obsRows,
		ConditionProblemsRows:     condRows,
	}, patID
}

func runBuiltInViewNormalized(t *testing.T, client *http.Client, base, viewID string, keepCols []string) []map[string]any {
	t.Helper()
	resp, body := mustSyntheaHTTP(t, client, http.MethodPost, base+"/ViewDefinition/"+viewID+"/$run?_format=application/json", "application/fhir+json",
		[]byte(`{"resourceType":"Parameters"}`), map[string]string{"Accept": "application/json"})
	require.Equal(t, http.StatusOK, resp.StatusCode, "ViewDefinition/%s/$run failed: %s", viewID, string(body))

	var rows []map[string]any
	require.NoError(t, json.Unmarshal(body, &rows))

	projected := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		m := make(map[string]any, len(keepCols))
		for _, col := range keepCols {
			m[col] = r[col]
		}
		projected = append(projected, m)
	}
	sort.Slice(projected, func(i, j int) bool {
		bi, _ := json.Marshal(projected[i])
		bj, _ := json.Marshal(projected[j])
		return string(bi) < string(bj)
	})
	return projected
}

func countBundleEntries(t *testing.T, client *http.Client, reqURL string) int {
	t.Helper()
	resp, body := mustSyntheaHTTP(t, client, http.MethodGet, reqURL, "", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "GET %s failed: %s", reqURL, string(body))
	var b struct {
		Entry []json.RawMessage `json:"entry"`
	}
	require.NoError(t, json.Unmarshal(body, &b))
	return len(b.Entry)
}

func mustSyntheaHTTP(t *testing.T, client *http.Client, method, reqURL, contentType string, body []byte, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, reqURL, rdr)
	require.NoError(t, err)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	respBytes, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	return resp, respBytes
}

func buildSyntheaUSCoreTransactionBundleJSON(t *testing.T) []byte {
	t.Helper()
	const (
		orgUUID  = "urn:uuid:11111111-1111-4111-8111-111111111101"
		locUUID  = "urn:uuid:11111111-1111-4111-8111-111111111102"
		pracUUID = "urn:uuid:11111111-1111-4111-8111-111111111103"
		patUUID  = "urn:uuid:11111111-1111-4111-8111-111111111104"
		encUUID  = "urn:uuid:11111111-1111-4111-8111-111111111105"
		condUUID = "urn:uuid:11111111-1111-4111-8111-111111111106"
		bpUUID   = "urn:uuid:11111111-1111-4111-8111-111111111107"
		a1cUUID  = "urn:uuid:11111111-1111-4111-8111-111111111108"
		medUUID  = "urn:uuid:11111111-1111-4111-8111-111111111109"
		immUUID  = "urn:uuid:11111111-1111-4111-8111-111111111110"
		allUUID  = "urn:uuid:11111111-1111-4111-8111-111111111111"
		procUUID = "urn:uuid:11111111-1111-4111-8111-111111111112"
		cpUUID   = "urn:uuid:11111111-1111-4111-8111-111111111113"
		drUUID   = "urn:uuid:11111111-1111-4111-8111-111111111114"
		docUUID  = "urn:uuid:11111111-1111-4111-8111-111111111115"
		provUUID = "urn:uuid:11111111-1111-4111-8111-111111111116"
	)

	noteBase64 := base64.StdEncoding.EncodeToString([]byte("Synthea Ambulatory Visit Note: Essential hypertension well-controlled on Lisinopril 10mg daily; HbA1c 6.4%."))

	entries := []struct {
		fullURL string
		resType string
		res     map[string]any
	}{
		{
			fullURL: orgUUID,
			resType: "Organization",
			res: map[string]any{
				"resourceType": "Organization",
				"active":       true,
				"name":         "Synthea Massachusetts General Health",
				"identifier": []map[string]any{
					{"system": "http://hl7.org/fhir/sid/us-npi", "value": "1234567893"},
				},
				"telecom": []map[string]any{
					{"system": "phone", "value": "617-555-0100"},
				},
				"address": []map[string]any{
					{"line": []string{"55 Fruit St"}, "city": "Boston", "state": "MA", "postalCode": "02114", "country": "US"},
				},
			},
		},
		{
			fullURL: locUUID,
			resType: "Location",
			res: map[string]any{
				"resourceType": "Location",
				"status":       "active",
				"name":         "Synthea Ambulatory Clinic Wing A",
				"address": map[string]any{
					"line": []string{"55 Fruit St"}, "city": "Boston", "state": "MA", "postalCode": "02114", "country": "US",
				},
				"managingOrganization": map[string]any{"reference": orgUUID},
			},
		},
		{
			fullURL: pracUUID,
			resType: "Practitioner",
			res: map[string]any{
				"resourceType": "Practitioner",
				"active":       true,
				"identifier": []map[string]any{
					{"system": "http://hl7.org/fhir/sid/us-npi", "value": "9999999993"},
				},
				"name": []map[string]any{
					{"family": "Reynolds", "given": []string{"Gregory"}, "prefix": []string{"Dr."}},
				},
			},
		},
		{
			fullURL: patUUID,
			resType: "Patient",
			res: map[string]any{
				"resourceType": "Patient",
				"meta": map[string]any{
					"profile": []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient"},
				},
				"extension": []map[string]any{
					{
						"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-race",
						"extension": []map[string]any{
							{"url": "ombCategory", "valueCoding": map[string]any{"system": "urn:oid:2.16.840.1.113883.6.238", "code": "2106-3", "display": "White"}},
							{"url": "text", "valueString": "White"},
						},
					},
					{
						"url": "http://hl7.org/fhir/us/core/StructureDefinition/us-core-ethnicity",
						"extension": []map[string]any{
							{"url": "ombCategory", "valueCoding": map[string]any{"system": "urn:oid:2.16.840.1.113883.6.238", "code": "2186-5", "display": "Not Hispanic or Latino"}},
							{"url": "text", "valueString": "Not Hispanic or Latino"},
						},
					},
					{
						"url":       "http://hl7.org/fhir/us/core/StructureDefinition/us-core-birthsex",
						"valueCode": "F",
					},
				},
				"identifier": []map[string]any{
					{
						"type": map[string]any{
							"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "MR"}},
						},
						"system": "https://github.com/synthetichealth/synthea",
						"value":  "SYNTHEA-MRN-0001",
					},
				},
				"active":    true,
				"name":      []map[string]any{{"use": "official", "family": "SyntheaParity", "given": []string{"Maria", "Elena"}}},
				"telecom":   []map[string]any{{"system": "phone", "value": "617-555-0199", "use": "home"}},
				"gender":    "female",
				"birthDate": "1978-09-14",
				"address": []map[string]any{
					{"line": []string{"742 Beacon St"}, "city": "Boston", "state": "MA", "postalCode": "02215", "country": "US"},
				},
			},
		},
		{
			fullURL: encUUID,
			resType: "Encounter",
			res: map[string]any{
				"resourceType": "Encounter",
				"status":       "finished",
				"class":        map[string]any{"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode", "code": "AMB", "display": "ambulatory"},
				"type": []map[string]any{
					{"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "185349003", "display": "Encounter for check up"}}},
				},
				"subject":         map[string]any{"reference": patUUID},
				"participant":     []map[string]any{{"individual": map[string]any{"reference": pracUUID}}},
				"period":          map[string]any{"start": "2026-02-15T09:00:00Z", "end": "2026-02-15T09:30:00Z"},
				"location":        []map[string]any{{"location": map[string]any{"reference": locUUID}}},
				"serviceProvider": map[string]any{"reference": orgUUID},
			},
		},
		{
			fullURL: condUUID,
			resType: "Condition",
			res: map[string]any{
				"resourceType":       "Condition",
				"clinicalStatus":     map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-clinical", "code": "active"}}},
				"verificationStatus": map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-ver-status", "code": "confirmed"}}},
				"category": []map[string]any{
					{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/condition-category", "code": "problem-list-item"}}},
				},
				"code": map[string]any{
					"coding": []map[string]any{
						{"system": "http://snomed.info/sct", "code": "59621000", "display": "Essential hypertension"},
						{"system": "http://hl7.org/fhir/sid/icd-10-cm", "code": "I10", "display": "Essential (primary) hypertension"},
					},
					"text": "Essential hypertension",
				},
				"subject":       map[string]any{"reference": patUUID},
				"encounter":     map[string]any{"reference": encUUID},
				"onsetDateTime": "2022-05-10T00:00:00Z",
				"recordedDate":  "2022-05-10T00:00:00Z",
			},
		},
		{
			fullURL: bpUUID,
			resType: "Observation",
			res: map[string]any{
				"resourceType": "Observation",
				"status":       "final",
				"category": []map[string]any{
					{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "vital-signs", "display": "Vital Signs"}}},
				},
				"code": map[string]any{
					"coding": []map[string]any{{"system": "http://loinc.org", "code": "85354-9", "display": "Blood pressure panel with all children optional"}},
					"text":   "Blood Pressure",
				},
				"subject":           map[string]any{"reference": patUUID},
				"encounter":         map[string]any{"reference": encUUID},
				"effectiveDateTime": "2026-02-15T09:10:00Z",
				"component": []map[string]any{
					{
						"code":          map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8480-6", "display": "Systolic blood pressure"}}},
						"valueQuantity": map[string]any{"value": 128.0, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"},
					},
					{
						"code":          map[string]any{"coding": []map[string]any{{"system": "http://loinc.org", "code": "8462-4", "display": "Diastolic blood pressure"}}},
						"valueQuantity": map[string]any{"value": 82.0, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"},
					},
				},
			},
		},
		{
			fullURL: a1cUUID,
			resType: "Observation",
			res: map[string]any{
				"resourceType": "Observation",
				"status":       "final",
				"category": []map[string]any{
					{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "laboratory", "display": "Laboratory"}}},
				},
				"code": map[string]any{
					"coding": []map[string]any{{"system": "http://loinc.org", "code": "4548-4", "display": "Hemoglobin A1c/Hemoglobin.total in Blood"}},
					"text":   "Hemoglobin A1c",
				},
				"subject":           map[string]any{"reference": patUUID},
				"encounter":         map[string]any{"reference": encUUID},
				"effectiveDateTime": "2026-02-15T09:15:00Z",
				"valueQuantity":     map[string]any{"value": 6.4, "unit": "%", "system": "http://unitsofmeasure.org", "code": "%"},
			},
		},
		{
			fullURL: medUUID,
			resType: "MedicationRequest",
			res: map[string]any{
				"resourceType": "MedicationRequest",
				"status":       "active",
				"intent":       "order",
				"medicationCodeableConcept": map[string]any{
					"coding": []map[string]any{{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "314076", "display": "Lisinopril 10 MG Oral Tablet"}},
					"text":   "Lisinopril 10 MG Oral Tablet",
				},
				"subject":    map[string]any{"reference": patUUID},
				"encounter":  map[string]any{"reference": encUUID},
				"authoredOn": "2026-02-15T09:20:00Z",
				"requester":  map[string]any{"reference": pracUUID},
			},
		},
		{
			fullURL: immUUID,
			resType: "Immunization",
			res: map[string]any{
				"resourceType": "Immunization",
				"status":       "completed",
				"vaccineCode": map[string]any{
					"coding": []map[string]any{{"system": "http://hl7.org/fhir/sid/cvx", "code": "140", "display": "Influenza, seasonal, injectable, preservative free"}},
				},
				"patient":            map[string]any{"reference": patUUID},
				"encounter":          map[string]any{"reference": encUUID},
				"occurrenceDateTime": "2026-02-15T09:22:00Z",
				"primarySource":      true,
			},
		},
		{
			fullURL: allUUID,
			resType: "AllergyIntolerance",
			res: map[string]any{
				"resourceType":       "AllergyIntolerance",
				"clinicalStatus":     map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/allergyintolerance-clinical", "code": "active"}}},
				"verificationStatus": map[string]any{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/allergyintolerance-verification", "code": "confirmed"}}},
				"code": map[string]any{
					"coding": []map[string]any{{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "7980", "display": "Penicillin G"}},
				},
				"patient": map[string]any{"reference": patUUID},
			},
		},
		{
			fullURL: procUUID,
			resType: "Procedure",
			res: map[string]any{
				"resourceType": "Procedure",
				"status":       "completed",
				"code": map[string]any{
					"coding": []map[string]any{{"system": "http://snomed.info/sct", "code": "430193006", "display": "Medication reconciliation (procedure)"}},
				},
				"subject":           map[string]any{"reference": patUUID},
				"encounter":         map[string]any{"reference": encUUID},
				"performedDateTime": "2026-02-15T09:25:00Z",
			},
		},
		{
			fullURL: cpUUID,
			resType: "CarePlan",
			res: map[string]any{
				"resourceType": "CarePlan",
				"status":       "active",
				"intent":       "plan",
				"category": []map[string]any{
					{"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/core/CodeSystem/careplan-category", "code": "assess-plan"}}},
				},
				"subject":   map[string]any{"reference": patUUID},
				"encounter": map[string]any{"reference": encUUID},
				"addresses": []map[string]any{{"reference": condUUID}},
			},
		},
		{
			fullURL: drUUID,
			resType: "DiagnosticReport",
			res: map[string]any{
				"resourceType": "DiagnosticReport",
				"status":       "final",
				"category": []map[string]any{
					{"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/v2-0074", "code": "LAB"}}},
				},
				"code": map[string]any{
					"coding": []map[string]any{{"system": "http://loinc.org", "code": "24323-8", "display": "Comprehensive metabolic 2000 panel - Serum or Plasma"}},
				},
				"subject":           map[string]any{"reference": patUUID},
				"encounter":         map[string]any{"reference": encUUID},
				"effectiveDateTime": "2026-02-15T09:15:00Z",
				"issued":            "2026-02-15T09:28:00Z",
				"result":            []map[string]any{{"reference": a1cUUID}},
			},
		},
		{
			fullURL: docUUID,
			resType: "DocumentReference",
			res: map[string]any{
				"resourceType": "DocumentReference",
				"status":       "current",
				"type": map[string]any{
					"coding": []map[string]any{{"system": "http://loinc.org", "code": "34117-2", "display": "History and physical note"}},
				},
				"category": []map[string]any{
					{"coding": []map[string]any{{"system": "http://hl7.org/fhir/us/core/CodeSystem/us-core-documentreference-category", "code": "clinical-note"}}},
				},
				"subject": map[string]any{"reference": patUUID},
				"date":    "2026-02-15T09:30:00Z",
				"author":  []map[string]any{{"reference": pracUUID}},
				"content": []map[string]any{
					{
						"attachment": map[string]any{
							"contentType": "text/plain; charset=utf-8",
							"data":        noteBase64,
						},
					},
				},
				"context": map[string]any{
					"encounter": []map[string]any{{"reference": encUUID}},
				},
			},
		},
		{
			fullURL: provUUID,
			resType: "Provenance",
			res: map[string]any{
				"resourceType": "Provenance",
				"target": []map[string]any{
					{"reference": patUUID},
					{"reference": docUUID},
				},
				"recorded": "2026-02-15T09:30:05Z",
				"agent": []map[string]any{
					{
						"type": map[string]any{
							"coding": []map[string]any{{"system": "http://terminology.hl7.org/CodeSystem/provenance-participant-type", "code": "author"}},
						},
						"who":        map[string]any{"reference": pracUUID},
						"onBehalfOf": map[string]any{"reference": orgUUID},
					},
				},
			},
		},
	}

	bundleEntries := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		bundleEntries = append(bundleEntries, map[string]any{
			"fullUrl":  e.fullURL,
			"resource": e.res,
			"request": map[string]any{
				"method": "POST",
				"url":    e.resType,
			},
		})
	}

	raw, err := json.Marshal(map[string]any{
		"resourceType": "Bundle",
		"type":         "transaction",
		"entry":        bundleEntries,
	})
	require.NoError(t, err)
	return raw
}

func TestOAuthDynamicClientRegistrationAndDevTokenEnforcement(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := sqlite.New(nil)
	srv := server.New(st, logger)
	server.RegisterDefaultResourceTypes(srv)
	srv.EnableDevTokenEnforcement()

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := ts.Client()

	// 1. Discovery exposes registration_endpoint and ES256/ES384 signing algorithms
	respDisc, bodyDisc := mustSyntheaHTTP(t, client, http.MethodGet, ts.URL+"/fhir/r4/demo/.well-known/smart-configuration", "", nil, nil)
	require.Equal(t, http.StatusOK, respDisc.StatusCode)
	var smartCfg map[string]any
	require.NoError(t, json.Unmarshal(bodyDisc, &smartCfg))
	assert.Equal(t, ts.URL+"/oauth2/register", smartCfg["registration_endpoint"])
	assert.Contains(t, smartCfg["token_endpoint_auth_signing_alg_values_supported"], "ES256")
	assert.Contains(t, smartCfg["token_endpoint_auth_signing_alg_values_supported"], "ES384")

	// 2. Unauthenticated clinical and Bulk Data $export requests return 401 when EnableDevTokenEnforcement is active
	respUnauthPat, _ := mustSyntheaHTTP(t, client, http.MethodGet, ts.URL+"/fhir/r4/demo/Patient", "", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, respUnauthPat.StatusCode)
	respUnauthExp, _ := mustSyntheaHTTP(t, client, http.MethodGet, ts.URL+"/fhir/r4/demo/Group/all/$export", "", nil, map[string]string{"Prefer": "respond-async"})
	assert.Equal(t, http.StatusUnauthorized, respUnauthExp.StatusCode)

	// 3. RFC 7591 Dynamic Client Registration error branches + valid ES384 JWKS registration
	respBadJSON, _ := mustSyntheaHTTP(t, client, http.MethodPost, ts.URL+"/oauth2/register", "application/json", []byte("{bad"), nil)
	assert.Equal(t, http.StatusBadRequest, respBadJSON.StatusCode)

	respBadJWK, _ := mustSyntheaHTTP(t, client, http.MethodPost, ts.URL+"/oauth2/register", "application/json",
		[]byte(`{"client_id":"bad","jwks":{"keys":[{"kty":"RSA","n":"***","e":"AQAB"}]}}`), nil)
	assert.Equal(t, http.StatusBadRequest, respBadJWK.StatusCode)

	respAutoID, bodyAutoID := mustSyntheaHTTP(t, client, http.MethodPost, ts.URL+"/oauth2/register", "application/json",
		[]byte(`{"client_name":"auto-client"}`), nil)
	require.Equal(t, http.StatusCreated, respAutoID.StatusCode)
	assert.Contains(t, string(bodyAutoID), `"flint-client-`)

	privKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	wrongKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)

	pubBytes, err := privKey.PublicKey.Bytes()
	require.NoError(t, err)
	require.Len(t, pubBytes, 1+48+48)
	jwk := auth.JWK{
		Kty: "EC",
		Crv: "P-384",
		Kid: "inferno-es384-key",
		Alg: "ES384",
		Use: "sig",
		X:   base64.RawURLEncoding.EncodeToString(pubBytes[1:49]),
		Y:   base64.RawURLEncoding.EncodeToString(pubBytes[49:97]),
	}
	regPayload, err := json.Marshal(map[string]any{
		"client_id":   "inferno-bulk-client",
		"client_name": "HL7 Inferno Headless Runner",
		"jwks":        auth.JWKSet{Keys: []auth.JWK{jwk}},
	})
	require.NoError(t, err)

	respReg, _ := mustSyntheaHTTP(t, client, http.MethodPost, ts.URL+"/oauth2/register", "application/json", regPayload, nil)
	require.Equal(t, http.StatusCreated, respReg.StatusCode)

	signES384Assertion := func(signer *ecdsa.PrivateKey, kid, jti string) string {
		hdr := map[string]string{"alg": "ES384", "typ": "JWT"}
		if kid != "" {
			hdr["kid"] = kid
		}
		hdrB, _ := json.Marshal(hdr)
		claimsB, _ := json.Marshal(map[string]any{
			"iss": "inferno-bulk-client",
			"sub": "inferno-bulk-client",
			"aud": ts.URL + "/oauth2/token",
			"exp": time.Now().Add(2 * time.Minute).Unix(),
			"jti": jti,
		})
		unsigned := base64.RawURLEncoding.EncodeToString(hdrB) + "." + base64.RawURLEncoding.EncodeToString(claimsB)
		digest := sha512.Sum384([]byte(unsigned))
		rSig, sSig, signErr := ecdsa.Sign(rand.Reader, signer, digest[:])
		require.NoError(t, signErr)
		sig := make([]byte, 96)
		rBytes := rSig.Bytes()
		sBytes := sSig.Bytes()
		copy(sig[48-len(rBytes):48], rBytes)
		copy(sig[96-len(sBytes):96], sBytes)
		return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig)
	}

	// 4. Wrong key fails verification against registered JWKSet (400 invalid_client)
	badForm := url.Values{
		"grant_type":            {"client_credentials"},
		"scope":                 {"system/*.rs"},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {signES384Assertion(wrongKey, "inferno-es384-key", "jti-bad-1")},
	}
	respBadTok, _ := mustSyntheaHTTP(t, client, http.MethodPost, ts.URL+"/oauth2/token", "application/x-www-form-urlencoded", []byte(badForm.Encode()), nil)
	assert.Equal(t, http.StatusBadRequest, respBadTok.StatusCode)

	// 5. Valid ES384 assertion (both with explicit kid and fallback empty kid) mints token that authorizes FHIR requests
	goodForm := url.Values{
		"grant_type":            {"client_credentials"},
		"scope":                 {"system/*.cruds"},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {signES384Assertion(privKey, "", "jti-good-1")},
	}
	respGoodTok, bodyGoodTok := mustSyntheaHTTP(t, client, http.MethodPost, ts.URL+"/oauth2/token", "application/x-www-form-urlencoded", []byte(goodForm.Encode()), nil)
	require.Equal(t, http.StatusOK, respGoodTok.StatusCode, string(bodyGoodTok))
	var tokResp struct {
		AccessToken string `json:"access_token"`
	}
	require.NoError(t, json.Unmarshal(bodyGoodTok, &tokResp))
	require.NotEmpty(t, tokResp.AccessToken)

	respAuthPat, _ := mustSyntheaHTTP(t, client, http.MethodGet, ts.URL+"/fhir/r4/demo/Patient", "", nil, map[string]string{
		"Authorization": "Bearer " + tokResp.AccessToken,
	})
	assert.Equal(t, http.StatusOK, respAuthPat.StatusCode)
}
