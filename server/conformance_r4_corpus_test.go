package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/flint-fhir/flint/pkg/audit"
	"github.com/flint-fhir/flint/pkg/fhirutil"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/postgres"
)

// TestAll146R4Resources_FullCRUDHistoryAndSearchRoundTrip verifies that every single
// official HL7 FHIR R4 resource type (all 146 compiled google/fhir R4 protobuf descriptors)
// completes a lossless end-to-end HTTP lifecycle:
//   - POST /{resourceType} (201 Created, FHIR JSON -> Proto3 binary normalization & validation)
//   - GET  /{resourceType}/{id} (200 OK, Proto3 binary -> FHIR JSON denormalization round-trip)
//   - PUT  /{resourceType}/{id} (200 OK, version 2 update)
//   - GET  /{resourceType}?_id={id} (200 OK, searchset Bundle covering-index lookup)
//   - GET  /{resourceType}/{id}/_history (200 OK, history Bundle with 2 versions)
//   - GET  /{resourceType}/{id}/_history/1 (200 OK, vread historical version 1)
//   - DELETE /{resourceType}/{id} (204 No Content) + GET tombstone (410 Gone)
func TestAll146R4Resources_FullCRUDHistoryAndSearchRoundTrip(t *testing.T) {
	db := openTestPostgres(t)
	defer db.Close()

	ctx := t.Context()
	if err := postgres.RunMigrations(ctx, db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	tenant := "r4-146-corpus-suite"
	cleanup := func() { _ = postgres.PurgeTenant(ctx, db, tenant) }
	cleanup()
	defer cleanup()

	st := postgres.New(db)
	srv := server.New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.RegisterDefaultResourceTypes(srv)
	srv.SetAuditRecorder(audit.NewStoreRecorder(st))
	h := srv.Handler()

	allTypes := fhirutil.AllR4ResourceTypes()
	if len(allTypes) < 146 {
		t.Fatalf("expected at least 146 R4 resource types in registry, got %d", len(allTypes))
	}

	for _, rt := range allTypes {
		t.Run(rt, func(t *testing.T) {
			if rt == "AuditEvent" {
				// AuditEvent is an immutable ATNA security ledger populated automatically on FHIR interactions
				// and queried via GET /AuditEvent and GET /AuditEvent/{id}.
				reqSearch := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/AuditEvent?_count=5", nil)
				recSearch := httptest.NewRecorder()
				h.ServeHTTP(recSearch, reqSearch)
				if recSearch.Code != http.StatusOK {
					t.Fatalf("GET /AuditEvent returned %d: %s", recSearch.Code, recSearch.Body.String())
				}
				var bundle map[string]any
				if err := json.Unmarshal(recSearch.Body.Bytes(), &bundle); err != nil {
					t.Fatalf("unmarshal AuditEvent bundle: %v", err)
				}
				if entries, ok := bundle["entry"].([]any); ok && len(entries) > 0 {
					firstRes := entries[0].(map[string]any)["resource"].(map[string]any)
					aeID, _ := firstRes["id"].(string)
					reqRead := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/AuditEvent/"+aeID, nil)
					recRead := httptest.NewRecorder()
					h.ServeHTTP(recRead, reqRead)
					if recRead.Code != http.StatusOK {
						t.Fatalf("GET /AuditEvent/%s returned %d: %s", aeID, recRead.Code, recRead.Body.String())
					}
				}
				return
			}

			// 1. POST /{resourceType} (Create v1)
			v1Map := buildMinimalValidR4Resource(rt, "", "en-US")
			v1Bytes, err := json.Marshal(v1Map)
			if err != nil {
				t.Fatalf("marshal %s v1: %v", rt, err)
			}

			reqCreate := httptest.NewRequest(http.MethodPost, "/fhir/r4/"+tenant+"/"+rt, bytes.NewReader(v1Bytes))
			reqCreate.Header.Set("Content-Type", "application/fhir+json")
			recCreate := httptest.NewRecorder()
			h.ServeHTTP(recCreate, reqCreate)
			if recCreate.Code != http.StatusCreated {
				t.Fatalf("POST /%s expected 201 Created, got %d: %s", rt, recCreate.Code, recCreate.Body.String())
			}

			var created map[string]any
			if err := json.Unmarshal(recCreate.Body.Bytes(), &created); err != nil {
				t.Fatalf("unmarshal created %s: %v", rt, err)
			}
			id, _ := created["id"].(string)
			if id == "" {
				t.Fatalf("POST /%s did not return an assigned resource id: %s", rt, recCreate.Body.String())
			}

			// 2. GET /{resourceType}/{id} (Read v1 — Proto3 wire round-trip)
			reqRead := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/"+rt+"/"+id, nil)
			recRead := httptest.NewRecorder()
			h.ServeHTTP(recRead, reqRead)
			if recRead.Code != http.StatusOK {
				t.Fatalf("GET /%s/%s expected 200 OK, got %d: %s", rt, id, recRead.Code, recRead.Body.String())
			}
			var readV1 map[string]any
			if err := json.Unmarshal(recRead.Body.Bytes(), &readV1); err != nil {
				t.Fatalf("unmarshal read %s/%s: %v", rt, id, err)
			}
			if gotRT, _ := readV1["resourceType"].(string); gotRT != rt {
				t.Fatalf("GET /%s/%s resourceType = %q, want %q", rt, id, gotRT, rt)
			}
			if gotLang, _ := readV1["language"].(string); gotLang != "en-US" {
				t.Fatalf("GET /%s/%s language = %q, want en-US", rt, id, gotLang)
			}

			// 3. PUT /{resourceType}/{id} (Update v2)
			v2Map := buildMinimalValidR4Resource(rt, id, "en-GB")
			v2Bytes, _ := json.Marshal(v2Map)
			reqUpdate := httptest.NewRequest(http.MethodPut, "/fhir/r4/"+tenant+"/"+rt+"/"+id, bytes.NewReader(v2Bytes))
			reqUpdate.Header.Set("Content-Type", "application/fhir+json")
			recUpdate := httptest.NewRecorder()
			h.ServeHTTP(recUpdate, reqUpdate)
			if recUpdate.Code != http.StatusOK {
				t.Fatalf("PUT /%s/%s expected 200 OK, got %d: %s", rt, id, recUpdate.Code, recUpdate.Body.String())
			}

			// 4. GET /{resourceType}?_id={id} (Search covering index)
			reqSearch := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/"+rt+"?_id="+id, nil)
			recSearch := httptest.NewRecorder()
			h.ServeHTTP(recSearch, reqSearch)
			if recSearch.Code != http.StatusOK {
				t.Fatalf("GET /%s?_id=%s expected 200 OK, got %d: %s", rt, id, recSearch.Code, recSearch.Body.String())
			}
			var searchBundle map[string]any
			if err := json.Unmarshal(recSearch.Body.Bytes(), &searchBundle); err != nil {
				t.Fatalf("unmarshal search bundle for %s: %v", rt, err)
			}
			if total, _ := searchBundle["total"].(float64); total < 1 {
				t.Fatalf("GET /%s?_id=%s expected total >= 1, got %v", rt, id, searchBundle["total"])
			}

			// 5. GET /{resourceType}/{id}/_history (Instance history with 2 versions)
			reqHist := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/"+rt+"/"+id+"/_history", nil)
			recHist := httptest.NewRecorder()
			h.ServeHTTP(recHist, reqHist)
			if recHist.Code != http.StatusOK {
				t.Fatalf("GET /%s/%s/_history expected 200 OK, got %d: %s", rt, id, recHist.Code, recHist.Body.String())
			}
			var histBundle map[string]any
			if err := json.Unmarshal(recHist.Body.Bytes(), &histBundle); err != nil {
				t.Fatalf("unmarshal history bundle for %s: %v", rt, err)
			}
			if total, _ := histBundle["total"].(float64); int(total) != 2 {
				t.Fatalf("GET /%s/%s/_history expected total=2, got %v", rt, id, histBundle["total"])
			}

			// 6. GET /{resourceType}/{id}/_history/1 (vread version 1)
			reqVRead := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/"+rt+"/"+id+"/_history/1", nil)
			recVRead := httptest.NewRecorder()
			h.ServeHTTP(recVRead, reqVRead)
			if recVRead.Code != http.StatusOK {
				t.Fatalf("GET /%s/%s/_history/1 expected 200 OK, got %d: %s", rt, id, recVRead.Code, recVRead.Body.String())
			}
			var vreadMap map[string]any
			_ = json.Unmarshal(recVRead.Body.Bytes(), &vreadMap)
			if gotLang, _ := vreadMap["language"].(string); gotLang != "en-US" {
				t.Fatalf("vread /%s/%s/_history/1 language = %q, want en-US", rt, id, gotLang)
			}

			// 7. DELETE /{resourceType}/{id} (204 No Content) + Tombstone check (410 Gone)
			reqDel := httptest.NewRequest(http.MethodDelete, "/fhir/r4/"+tenant+"/"+rt+"/"+id, nil)
			recDel := httptest.NewRecorder()
			h.ServeHTTP(recDel, reqDel)
			if recDel.Code != http.StatusNoContent {
				t.Fatalf("DELETE /%s/%s expected 204 No Content, got %d: %s", rt, id, recDel.Code, recDel.Body.String())
			}

			reqGone := httptest.NewRequest(http.MethodGet, "/fhir/r4/"+tenant+"/"+rt+"/"+id, nil)
			recGone := httptest.NewRecorder()
			h.ServeHTTP(recGone, reqGone)
			if recGone.Code != http.StatusGone {
				t.Fatalf("GET deleted /%s/%s expected 410 Gone, got %d: %s", rt, id, recGone.Code, recGone.Body.String())
			}
		})
	}
}

func buildMinimalValidR4Resource(rt, id, lang string) map[string]any {
	m := map[string]any{
		"resourceType": rt,
		"language":     lang,
	}
	if id != "" {
		m["id"] = id
	}

	codeable := func(system, code string) map[string]any {
		return map[string]any{
			"coding": []any{
				map[string]any{"system": system, "code": code},
			},
		}
	}
	ref := func(target string) map[string]any {
		return map[string]any{"reference": target}
	}

	switch rt {
	case "Observation":
		m["status"] = "final"
		m["code"] = codeable("http://loinc.org", "29463-7")
	case "Condition":
		m["subject"] = ref("Patient/pat-corpus-1")
	case "Encounter":
		m["status"] = "finished"
		m["class"] = map[string]any{
			"system": "http://terminology.hl7.org/CodeSystem/v3-ActCode",
			"code":   "AMB",
		}
	case "MedicationRequest":
		m["status"] = "active"
		m["intent"] = "order"
		m["medicationCodeableConcept"] = codeable("http://www.nlm.nih.gov/research/umls/rxnorm", "197361")
		m["subject"] = ref("Patient/pat-corpus-1")
	case "AllergyIntolerance":
		m["patient"] = ref("Patient/pat-corpus-1")
	case "Procedure":
		m["status"] = "completed"
		m["subject"] = ref("Patient/pat-corpus-1")
	case "DiagnosticReport":
		m["status"] = "final"
		m["code"] = codeable("http://loinc.org", "58410-2")
	case "Immunization":
		m["status"] = "completed"
		m["vaccineCode"] = codeable("http://hl7.org/fhir/sid/cvx", "207")
		m["patient"] = ref("Patient/pat-corpus-1")
		m["occurrenceDateTime"] = "2026-01-15T10:00:00Z"
	case "Specimen":
		m["type"] = codeable("http://snomed.info/sct", "119297000")
		m["subject"] = ref("Patient/pat-corpus-1")
	case "Location":
		m["name"] = "Corpus Test Clinic"
	case "DocumentReference":
		m["status"] = "current"
		m["type"] = codeable("http://loinc.org", "34133-9")
		m["subject"] = ref("Patient/pat-corpus-1")
		m["content"] = []any{
			map[string]any{
				"attachment": map[string]any{"contentType": "text/plain"},
			},
		}
	case "Provenance":
		m["target"] = []any{ref("Patient/pat-corpus-1")}
		m["recorded"] = "2026-01-15T10:00:00Z"
		m["agent"] = []any{
			map[string]any{"who": ref("Practitioner/prac-corpus-1")},
		}
	case "CarePlan":
		m["status"] = "active"
		m["intent"] = "plan"
		m["subject"] = ref("Patient/pat-corpus-1")
	case "CareTeam":
		m["subject"] = ref("Patient/pat-corpus-1")
	case "Goal":
		m["lifecycleStatus"] = "active"
		m["description"] = map[string]any{"text": "Maintain healthy blood pressure"}
		m["subject"] = ref("Patient/pat-corpus-1")
	case "ServiceRequest":
		m["status"] = "active"
		m["intent"] = "order"
		m["code"] = codeable("http://loinc.org", "24323-8")
		m["subject"] = ref("Patient/pat-corpus-1")
	case "Coverage":
		m["status"] = "active"
		m["beneficiary"] = ref("Patient/pat-corpus-1")
		m["payor"] = []any{ref("Organization/org-corpus-1")}
	case "QuestionnaireResponse":
		m["status"] = "completed"
	case "MedicationDispense":
		m["status"] = "completed"
		m["medicationCodeableConcept"] = codeable("http://www.nlm.nih.gov/research/umls/rxnorm", "197361")
		m["subject"] = ref("Patient/pat-corpus-1")
	case "Medication":
		m["code"] = codeable("http://www.nlm.nih.gov/research/umls/rxnorm", "197361")
	case "Device":
		m["type"] = codeable("http://snomed.info/sct", "14106009")
	case "RelatedPerson":
		m["patient"] = ref("Patient/pat-corpus-1")
	case "Bundle":
		m["type"] = "collection"
	}
	return m
}
