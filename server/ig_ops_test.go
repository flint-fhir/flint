package server_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/flint-fhir/flint/pkg/audit"
	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store"
	"github.com/flint-fhir/flint/store/sqlite"
)

func buildTestTGZBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	for name, content := range files {
		raw := []byte(content)
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name:     name,
			Typeflag: tar.TypeReg,
			Mode:     0o644,
			Size:     int64(len(raw)),
		}))
		_, err := tw.Write(raw)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())
	return buf.Bytes()
}

func newSQLiteBackedServer(t *testing.T) *server.Server {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "ig-test.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, sqlite.RunMigrations(context.Background(), db, sqlite.JournalWAL))
	st := sqlite.New(db, sqlite.WithJournalMode(sqlite.JournalWAL), sqlite.WithDSN(dbPath))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(st, logger)
	server.RegisterDefaultResourceTypes(srv)
	return srv
}

func TestIGPackageInstall_EndToEndConformanceSearchTerminologyValidationAndReindex(t *testing.T) {
	srv := newSQLiteBackedServer(t)
	h := srv.Handler()

	// 1. Seed a pre-existing Patient carrying a custom extension BEFORE installing the IG,
	// so we can prove $install-ig with reindex=true backfills custom SearchParameter indexes.
	preExistingPat := `{
		"resourceType": "Patient",
		"id": "pat-pre-ig",
		"active": true,
		"name": [{"family": "curie", "given": ["marie"]}],
		"extension": [{
			"url": "http://acme.org/fhir/ig/cardio/StructureDefinition/cardio-risk-ext",
			"valueCode": "high"
		}]
	}`
	reqPre := httptest.NewRequest(http.MethodPut, "/fhir/r4/default/Patient/pat-pre-ig", strings.NewReader(preExistingPat))
	reqPre.Header.Set("Content-Type", "application/fhir+json")
	recPre := httptest.NewRecorder()
	h.ServeHTTP(recPre, reqPre)
	require.Equal(t, http.StatusCreated, recPre.Code, recPre.Body.String())

	// 2. Build two interdependent FHIR R4 NPM (.tgz) packages:
	//    - acme.cardio.base@1.0.0 (terminology + base SearchParameter)
	//    - acme.cardio.app@2.0.0 (depends on acme.cardio.base, defines Observation profile + CapabilityStatement + IG)
	baseTGZ := buildTestTGZBytes(t, map[string]string{
		"package/package.json": `{
			"name": "acme.cardio.base",
			"version": "1.0.0",
			"canonical": "http://acme.org/fhir/ig/cardio-base",
			"fhirVersions": ["4.0.1"],
			"title": "Acme Cardio Base Terminology & Search"
		}`,
		"package/CodeSystem-cardio-risk-cs.json": `{
			"resourceType": "CodeSystem",
			"id": "cardio-risk-cs",
			"url": "http://acme.org/fhir/ig/cardio/CodeSystem/cardio-risk",
			"version": "1.0.0",
			"name": "CardioRiskCS",
			"status": "active",
			"content": "complete",
			"concept": [
				{"code": "low", "display": "Low Cardiovascular Risk"},
				{
					"code": "elevated",
					"display": "Elevated Cardiovascular Risk",
					"concept": [
						{
							"code": "high",
							"display": "High Cardiovascular Risk",
							"concept": [
								{"code": "critical", "display": "Critical Cardiovascular Risk"}
							]
						}
					]
				}
			]
		}`,
		"package/ValueSet-cardio-risk-vs.json": `{
			"resourceType": "ValueSet",
			"id": "cardio-risk-vs",
			"url": "http://acme.org/fhir/ig/cardio/ValueSet/cardio-risk-vs",
			"version": "1.0.0",
			"name": "CardioRiskVS",
			"status": "active",
			"compose": {
				"include": [
					{"system": "http://acme.org/fhir/ig/cardio/CodeSystem/cardio-risk"}
				]
			}
		}`,
		"package/ConceptMap-cardio-risk-map.json": `{
			"resourceType": "ConceptMap",
			"id": "cardio-risk-map",
			"url": "http://acme.org/fhir/ig/cardio/ConceptMap/cardio-risk-to-snomed",
			"version": "1.0.0",
			"name": "CardioRiskToSNOMED",
			"status": "active",
			"group": [
				{
					"source": "http://acme.org/fhir/ig/cardio/CodeSystem/cardio-risk",
					"target": "http://snomed.info/sct",
					"element": [
						{
							"code": "high",
							"display": "High Cardiovascular Risk",
							"target": [
								{
									"code": "723509005",
									"display": "High risk of cardiovascular disease",
									"equivalence": "equivalent"
								}
							]
						}
					]
				}
			]
		}`,
		"package/SearchParameter-patient-cardio-risk-tier.json": `{
			"resourceType": "SearchParameter",
			"id": "patient-cardio-risk-tier",
			"url": "http://acme.org/fhir/ig/cardio/SearchParameter/patient-cardio-risk-tier",
			"version": "1.0.0",
			"name": "PatientCardioRiskTier",
			"status": "active",
			"code": "cardio-risk-tier",
			"base": ["Patient"],
			"type": "token",
			"expression": "Patient.extension('http://acme.org/fhir/ig/cardio/StructureDefinition/cardio-risk-ext').value"
		}`,
	})

	appTGZ := buildTestTGZBytes(t, map[string]string{
		"package/package.json": `{
			"name": "acme.cardio.app",
			"version": "2.0.0",
			"canonical": "http://acme.org/fhir/ig/cardio-app",
			"fhirVersions": ["4.0.1"],
			"title": "Acme Cardio Clinical Profiles",
			"dependencies": {
				"acme.cardio.base": "1.0.0"
			}
		}`,
		"package/StructureDefinition-cardio-risk-observation.json": `{
			"resourceType": "StructureDefinition",
			"id": "cardio-risk-observation",
			"url": "http://acme.org/fhir/ig/cardio/StructureDefinition/cardio-risk-observation",
			"version": "2.0.0",
			"name": "CardioRiskObservation",
			"status": "active",
			"kind": "resource",
			"abstract": false,
			"type": "Observation",
			"baseDefinition": "http://hl7.org/fhir/StructureDefinition/Observation",
			"derivation": "constraint",
			"differential": {
				"element": [
					{
						"id": "Observation.value[x]",
						"path": "Observation.value[x]",
						"min": 1,
						"binding": {
							"strength": "required",
							"valueSet": "http://acme.org/fhir/ig/cardio/ValueSet/cardio-risk-vs|1.0.0"
						}
					}
				]
			}
		}`,
		"package/StructureDefinition-cardio-detected-issue.json": `{
			"resourceType": "StructureDefinition",
			"id": "cardio-detected-issue",
			"url": "http://acme.org/fhir/ig/cardio/StructureDefinition/cardio-detected-issue",
			"version": "2.0.0",
			"name": "CardioDetectedIssue",
			"status": "active",
			"kind": "resource",
			"abstract": false,
			"type": "DetectedIssue",
			"baseDefinition": "http://hl7.org/fhir/StructureDefinition/DetectedIssue",
			"derivation": "constraint"
		}`,
		"package/CapabilityStatement-cardio-server.json": `{
			"resourceType": "CapabilityStatement",
			"id": "cardio-server",
			"url": "http://acme.org/fhir/ig/cardio/CapabilityStatement/cardio-server",
			"version": "2.0.0",
			"name": "CardioServerCap",
			"status": "active",
			"date": "2026-01-01",
			"kind": "requirements",
			"fhirVersion": "4.0.1",
			"format": ["application/fhir+json"],
			"rest": [
				{
					"mode": "server",
					"resource": [
						{
							"type": "Patient",
							"interaction": [{"code": "read"}, {"code": "search-type"}],
							"searchParam": [{"name": "cardio-risk-tier", "type": "token"}]
						},
						{
							"type": "Observation",
							"interaction": [{"code": "read"}, {"code": "create"}, {"code": "search-type"}]
						}
					]
				}
			]
		}`,
		"package/ImplementationGuide-acme-cardio.json": `{
			"resourceType": "ImplementationGuide",
			"id": "acme-cardio",
			"url": "http://acme.org/fhir/ig/cardio/ImplementationGuide/acme-cardio",
			"version": "2.0.0",
			"name": "AcmeCardioIG",
			"status": "active",
			"packageId": "acme.cardio.app",
			"fhirVersion": ["4.0.1"]
		}`,
		"package/examples/Patient-cardio-ex-1.json": `{
			"resourceType": "Patient",
			"id": "cardio-ex-1",
			"active": true,
			"name": [{"family": "pasteur", "given": ["louis"]}]
		}`,
	})

	// 3. Submit BOTH packages via POST /fhir/r4/default/$install-ig in REVERSE dependency order
	// (app before base) with reindex=true and loadExamples=true.
	installParams := map[string]any{
		"resourceType": "Parameters",
		"parameter": []map[string]any{
			{"name": "reindex", "valueBoolean": true},
			{"name": "loadExamples", "valueBoolean": true},
			{"name": "npmPackage", "valueBase64Binary": base64.StdEncoding.EncodeToString(appTGZ)},
			{"name": "npmPackage", "valueBase64Binary": base64.StdEncoding.EncodeToString(baseTGZ)},
		},
	}
	rawInstallBody, err := json.Marshal(installParams)
	require.NoError(t, err)

	reqInstall := httptest.NewRequest(http.MethodPost, "/fhir/r4/default/$install-ig", bytes.NewReader(rawInstallBody))
	reqInstall.Header.Set("Content-Type", "application/fhir+json")
	recInstall := httptest.NewRecorder()
	h.ServeHTTP(recInstall, reqInstall)
	require.Equal(t, http.StatusOK, recInstall.Code, recInstall.Body.String())

	var installResp map[string]any
	require.NoError(t, json.Unmarshal(recInstall.Body.Bytes(), &installResp))
	assert.Equal(t, "Parameters", installResp["resourceType"])

	installed := srv.InstalledIGs()
	require.Len(t, installed, 2)
	// Topological sort must install acme.cardio.base BEFORE acme.cardio.app!
	assert.Equal(t, "acme.cardio.base", installed[0].Name)
	assert.Equal(t, "acme.cardio.app", installed[1].Name)
	assert.GreaterOrEqual(t, installed[0].ReindexedResources, 1, "pre-existing Patient should have been reindexed")
	assert.Equal(t, 1, installed[1].TotalExamples)

	// 4. Verify custom SearchParameter works immediately on the PRE-EXISTING Patient (via automatic $reindex)
	//    AND with hierarchical :below modifier!
	reqSearchPre := httptest.NewRequest(http.MethodGet, "/fhir/r4/default/Patient?cardio-risk-tier=high", nil)
	recSearchPre := httptest.NewRecorder()
	h.ServeHTTP(recSearchPre, reqSearchPre)
	require.Equal(t, http.StatusOK, recSearchPre.Code, recSearchPre.Body.String())
	assert.Contains(t, recSearchPre.Body.String(), `"pat-pre-ig"`)

	// Also verify example Patient from package/examples/ was persisted because loadExamples=true
	reqEx := httptest.NewRequest(http.MethodGet, "/fhir/r4/default/Patient/cardio-ex-1", nil)
	recEx := httptest.NewRecorder()
	h.ServeHTTP(recEx, reqEx)
	require.Equal(t, http.StatusOK, recEx.Code, recEx.Body.String())
	assert.Contains(t, recEx.Body.String(), `"pasteur"`)

	// 5. Verify Terminology Operations: ValueSet/$expand, ValueSet/$validate-code,
	//    CodeSystem/$lookup, CodeSystem/$subsumes, and ConceptMap/$translate!
	reqExpand := httptest.NewRequest(
		http.MethodGet,
		"/fhir/r4/default/ValueSet/$expand?url=http://acme.org/fhir/ig/cardio/ValueSet/cardio-risk-vs|1.0.0",
		nil,
	)
	recExpand := httptest.NewRecorder()
	h.ServeHTTP(recExpand, reqExpand)
	require.Equal(t, http.StatusOK, recExpand.Code, recExpand.Body.String())
	assert.Contains(t, recExpand.Body.String(), `"critical"`)
	assert.Contains(t, recExpand.Body.String(), `"elevated"`)

	reqValCode := httptest.NewRequest(
		http.MethodGet,
		"/fhir/r4/default/ValueSet/cardio-risk-vs/$validate-code?system=http://acme.org/fhir/ig/cardio/CodeSystem/cardio-risk&code=high",
		nil,
	)
	recValCode := httptest.NewRecorder()
	h.ServeHTTP(recValCode, reqValCode)
	require.Equal(t, http.StatusOK, recValCode.Code, recValCode.Body.String())
	assert.Contains(t, recValCode.Body.String(), `"valueBoolean":true`)

	reqLookup := httptest.NewRequest(
		http.MethodGet,
		"/fhir/r4/default/CodeSystem/$lookup?system=http://acme.org/fhir/ig/cardio/CodeSystem/cardio-risk&code=critical",
		nil,
	)
	recLookup := httptest.NewRecorder()
	h.ServeHTTP(recLookup, reqLookup)
	require.Equal(t, http.StatusOK, recLookup.Code, recLookup.Body.String())
	assert.Contains(t, recLookup.Body.String(), `"Critical Cardiovascular Risk"`)

	reqSubsumes := httptest.NewRequest(
		http.MethodGet,
		"/fhir/r4/default/CodeSystem/$subsumes?system=http://acme.org/fhir/ig/cardio/CodeSystem/cardio-risk&codeA=elevated&codeB=critical",
		nil,
	)
	recSubsumes := httptest.NewRecorder()
	h.ServeHTTP(recSubsumes, reqSubsumes)
	require.Equal(t, http.StatusOK, recSubsumes.Code, recSubsumes.Body.String())
	assert.Contains(t, recSubsumes.Body.String(), `"subsumes"`)

	reqTranslate := httptest.NewRequest(
		http.MethodGet,
		"/fhir/r4/default/ConceptMap/$translate?url=http://acme.org/fhir/ig/cardio/ConceptMap/cardio-risk-to-snomed|1.0.0&system=http://acme.org/fhir/ig/cardio/CodeSystem/cardio-risk&code=high",
		nil,
	)
	recTranslate := httptest.NewRecorder()
	h.ServeHTTP(recTranslate, reqTranslate)
	require.Equal(t, http.StatusOK, recTranslate.Code, recTranslate.Body.String())
	assert.Contains(t, recTranslate.Body.String(), `"723509005"`)

	// 6. Verify Profile Validation ($validate & write-time meta.profile enforcement with versioned canonical
	//    AND required ValueSet binding enforcement)!
	validObs := `{
		"resourceType": "Observation",
		"meta": {"profile": ["http://acme.org/fhir/ig/cardio/StructureDefinition/cardio-risk-observation|2.0.0"]},
		"status": "final",
		"code": {"coding": [{"system": "http://loinc.org", "code": "85354-9"}]},
		"subject": {"reference": "Patient/pat-pre-ig"},
		"valueCodeableConcept": {
			"coding": [{
				"system": "http://acme.org/fhir/ig/cardio/CodeSystem/cardio-risk",
				"code": "critical"
			}]
		}
	}`
	reqCreateValidObs := httptest.NewRequest(http.MethodPost, "/fhir/r4/default/Observation", strings.NewReader(validObs))
	reqCreateValidObs.Header.Set("Content-Type", "application/fhir+json")
	recCreateValidObs := httptest.NewRecorder()
	h.ServeHTTP(recCreateValidObs, reqCreateValidObs)
	require.Equal(t, http.StatusCreated, recCreateValidObs.Code, recCreateValidObs.Body.String())

	invalidBindingObs := `{
		"resourceType": "Observation",
		"meta": {"profile": ["http://acme.org/fhir/ig/cardio/StructureDefinition/cardio-risk-observation|2.0.0"]},
		"status": "final",
		"code": {"coding": [{"system": "http://loinc.org", "code": "85354-9"}]},
		"subject": {"reference": "Patient/pat-pre-ig"},
		"valueCodeableConcept": {
			"coding": [{
				"system": "http://acme.org/fhir/ig/cardio/CodeSystem/cardio-risk",
				"code": "unknown-risk-level"
			}]
		}
	}`
	reqCreateInvalidObs := httptest.NewRequest(http.MethodPost, "/fhir/r4/default/Observation", strings.NewReader(invalidBindingObs))
	reqCreateInvalidObs.Header.Set("Content-Type", "application/fhir+json")
	recCreateInvalidObs := httptest.NewRecorder()
	h.ServeHTTP(recCreateInvalidObs, reqCreateInvalidObs)
	require.Equal(t, http.StatusBadRequest, recCreateInvalidObs.Code, recCreateInvalidObs.Body.String())
	assert.Contains(t, recCreateInvalidObs.Body.String(), "ValueSet")

	// Also verify $validate explicitly enforces the versioned IG profile!
	reqValidateOp := httptest.NewRequest(
		http.MethodPost,
		"/fhir/r4/default/Observation/$validate?profile=http://acme.org/fhir/ig/cardio/StructureDefinition/cardio-risk-observation|2.0.0",
		strings.NewReader(invalidBindingObs),
	)
	reqValidateOp.Header.Set("Content-Type", "application/fhir+json")
	recValidateOp := httptest.NewRecorder()
	h.ServeHTTP(recValidateOp, reqValidateOp)
	require.Equal(t, http.StatusBadRequest, recValidateOp.Code, recValidateOp.Body.String())
	assert.Contains(t, recValidateOp.Body.String(), `"severity":"error"`)
	assert.Contains(t, recValidateOp.Body.String(), "ValueSet")

	reqValidateValidOp := httptest.NewRequest(
		http.MethodPost,
		"/fhir/r4/default/Observation/$validate?profile=http://acme.org/fhir/ig/cardio/StructureDefinition/cardio-risk-observation|2.0.0",
		strings.NewReader(validObs),
	)
	reqValidateValidOp.Header.Set("Content-Type", "application/fhir+json")
	recValidateValidOp := httptest.NewRecorder()
	h.ServeHTTP(recValidateValidOp, reqValidateValidOp)
	require.Equal(t, http.StatusOK, recValidateValidOp.Code, recValidateValidOp.Body.String())

	// 7. Verify StructureDefinition/$snapshot, CapabilityStatement, CapabilityStatement/$implements, and GET /$igs!
	reqSnap := httptest.NewRequest(
		http.MethodGet,
		"/fhir/r4/default/StructureDefinition/$snapshot?url=http://acme.org/fhir/ig/cardio/StructureDefinition/cardio-risk-observation",
		nil,
	)
	recSnap := httptest.NewRecorder()
	h.ServeHTTP(recSnap, reqSnap)
	require.Equal(t, http.StatusOK, recSnap.Code, recSnap.Body.String())
	assert.Contains(t, recSnap.Body.String(), `"snapshot"`)

	reqMeta := httptest.NewRequest(http.MethodGet, "/fhir/r4/default/metadata", nil)
	recMeta := httptest.NewRecorder()
	h.ServeHTTP(recMeta, reqMeta)
	require.Equal(t, http.StatusOK, recMeta.Code, recMeta.Body.String())
	assert.Contains(t, recMeta.Body.String(), `"http://acme.org/fhir/ig/cardio/ImplementationGuide/acme-cardio"`)
	assert.Contains(t, recMeta.Body.String(), `"http://acme.org/fhir/ig/cardio/StructureDefinition/cardio-risk-observation"`)
	assert.Contains(t, recMeta.Body.String(), `"http://acme.org/fhir/ig/cardio/StructureDefinition/cardio-detected-issue"`)
	assert.Contains(t, recMeta.Body.String(), `"cardio-risk-tier"`)
	assert.Contains(t, recMeta.Body.String(), `"install-ig"`)

	reqImpl := httptest.NewRequest(
		http.MethodGet,
		"/fhir/r4/default/CapabilityStatement/$implements?canonical=http://acme.org/fhir/ig/cardio/CapabilityStatement/cardio-server",
		nil,
	)
	recImpl := httptest.NewRecorder()
	h.ServeHTTP(recImpl, reqImpl)
	require.Equal(t, http.StatusOK, recImpl.Code, recImpl.Body.String())
	assert.Contains(t, recImpl.Body.String(), `"valueBoolean":true`)

	reqListSys := httptest.NewRequest(http.MethodGet, "/fhir/r4/$igs", nil)
	recListSys := httptest.NewRecorder()
	h.ServeHTTP(recListSys, reqListSys)
	require.Equal(t, http.StatusOK, recListSys.Code, recListSys.Body.String())
	assert.Contains(t, recListSys.Body.String(), `"acme.cardio.base"`)
	assert.Contains(t, recListSys.Body.String(), `"acme.cardio.app"`)
}

func TestIGPackageInstall_StatelessFormatsPayloadVariantsAndErrorHandling(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(nil, logger)
	server.RegisterDefaultResourceTypes(srv)
	h := srv.Handler()

	tgzBytes := buildTestTGZBytes(t, map[string]string{
		"package/package.json": `{
			"name": "stateless.neuro.ig",
			"version": "1.2.0",
			"canonical": "http://example.org/fhir/ig/neuro",
			"fhirVersions": ["4.0.1"]
		}`,
		"package/CodeSystem-neuro-cs.json": `{
			"resourceType": "CodeSystem",
			"id": "neuro-cs",
			"url": "http://example.org/fhir/ig/neuro/CodeSystem/neuro-scale",
			"version": "1.2.0",
			"status": "active",
			"content": "complete",
			"concept": [{"code": "nihss-0", "display": "No Stroke Symptoms"}]
		}`,
		"package/ValueSet-neuro-vs.json": `{
			"resourceType": "ValueSet",
			"id": "neuro-vs",
			"url": "http://example.org/fhir/ig/neuro/ValueSet/neuro-scale-vs",
			"version": "1.2.0",
			"status": "active",
			"compose": {"include": [{"system": "http://example.org/fhir/ig/neuro/CodeSystem/neuro-scale"}]}
		}`,
		"package/ConceptMap-neuro-cm.json": `{
			"resourceType": "ConceptMap",
			"id": "neuro-cm",
			"url": "http://example.org/fhir/ig/neuro/ConceptMap/neuro-map",
			"version": "1.2.0",
			"status": "active",
			"group": [{
				"source": "http://example.org/fhir/ig/neuro/CodeSystem/neuro-scale",
				"target": "http://snomed.info/sct",
				"element": [{"code": "nihss-0", "target": [{"code": "17621005", "equivalence": "equivalent"}]}]
			}]
		}`,
	})

	// 1. Raw binary .tgz upload on system-level /fhir/r4/$install-ig
	reqRaw := httptest.NewRequest(http.MethodPost, "/fhir/r4/$install-ig", bytes.NewReader(tgzBytes))
	reqRaw.Header.Set("Content-Type", "application/gzip")
	recRaw := httptest.NewRecorder()
	h.ServeHTTP(recRaw, reqRaw)
	require.Equal(t, http.StatusOK, recRaw.Code, recRaw.Body.String())

	// Verify stateless in-memory terminology resolution works with s.store == nil!
	reqExp := httptest.NewRequest(http.MethodGet, "/fhir/r4/any-tenant/ValueSet/neuro-vs/$expand", nil)
	recExp := httptest.NewRecorder()
	h.ServeHTTP(recExp, reqExp)
	require.Equal(t, http.StatusOK, recExp.Code, recExp.Body.String())
	assert.Contains(t, recExp.Body.String(), `"nihss-0"`)

	reqCMByID := httptest.NewRequest(
		http.MethodGet,
		"/fhir/r4/any-tenant/ConceptMap/neuro-cm/$translate?system=http://example.org/fhir/ig/neuro/CodeSystem/neuro-scale&code=nihss-0",
		nil,
	)
	recCMByID := httptest.NewRecorder()
	h.ServeHTTP(recCMByID, reqCMByID)
	require.Equal(t, http.StatusOK, recCMByID.Code, recCMByID.Body.String())
	assert.Contains(t, recCMByID.Body.String(), `"17621005"`)

	reqCMUnkeyed := httptest.NewRequest(
		http.MethodGet,
		"/fhir/r4/any-tenant/ConceptMap/$translate?system=http://example.org/fhir/ig/neuro/CodeSystem/neuro-scale&code=nihss-0",
		nil,
	)
	recCMUnkeyed := httptest.NewRecorder()
	h.ServeHTTP(recCMUnkeyed, reqCMUnkeyed)
	require.Equal(t, http.StatusOK, recCMUnkeyed.Code, recCMUnkeyed.Body.String())
	assert.Contains(t, recCMUnkeyed.Body.String(), `"17621005"`)

	// 2. FHIR Binary payload on $load-ig alias (re-installing updates in place without duplicating InstalledIGs)
	binPayload := `{"resourceType":"Binary","contentType":"application/gzip","data":"` + base64.StdEncoding.EncodeToString(tgzBytes) + `"}`
	reqBin := httptest.NewRequest(http.MethodPost, "/fhir/r4/$load-ig", strings.NewReader(binPayload))
	reqBin.Header.Set("Content-Type", "application/fhir+json")
	recBin := httptest.NewRecorder()
	h.ServeHTTP(recBin, reqBin)
	require.Equal(t, http.StatusOK, recBin.Code, recBin.Body.String())
	assert.Len(t, srv.InstalledIGs(), 1, "reinstalling same package@version should replace summary in place")

	// 3. Parameters with valueAttachment and embedded Binary resource
	attPayload := `{"resourceType":"Parameters","parameter":[{"name":"package","valueAttachment":{"data":"` + base64.StdEncoding.EncodeToString(tgzBytes) + `"}}]}`
	reqAtt := httptest.NewRequest(http.MethodPost, "/fhir/r4/default/$install-ig", strings.NewReader(attPayload))
	reqAtt.Header.Set("Content-Type", "application/fhir+json")
	recAtt := httptest.NewRecorder()
	h.ServeHTTP(recAtt, reqAtt)
	require.Equal(t, http.StatusOK, recAtt.Code, recAtt.Body.String())

	embBinPayload := `{"resourceType":"Parameters","parameter":[{"name":"npmPackage","resource":{"resourceType":"Binary","data":"` + base64.StdEncoding.EncodeToString(tgzBytes) + `"}}]}`
	reqEmb := httptest.NewRequest(http.MethodPost, "/fhir/r4/default/$install-ig", strings.NewReader(embBinPayload))
	reqEmb.Header.Set("Content-Type", "application/fhir+json")
	recEmb := httptest.NewRecorder()
	h.ServeHTTP(recEmb, reqEmb)
	require.Equal(t, http.StatusOK, recEmb.Code, recEmb.Body.String())

	// 4. Local filesystem 'path' parameter is blocked over HTTP by default (403), then succeeds when explicitly enabled
	diskFile := filepath.Join(t.TempDir(), "neuro.tgz")
	require.NoError(t, os.WriteFile(diskFile, tgzBytes, 0o600))
	pathPayload := `{"resourceType":"Parameters","parameter":[{"name":"path","valueString":` + mustJSONQuote(diskFile) + `}]}`

	reqPathBlocked := httptest.NewRequest(http.MethodPost, "/fhir/r4/$install-ig", strings.NewReader(pathPayload))
	reqPathBlocked.Header.Set("Content-Type", "application/fhir+json")
	recPathBlocked := httptest.NewRecorder()
	h.ServeHTTP(recPathBlocked, reqPathBlocked)
	assert.Equal(t, http.StatusForbidden, recPathBlocked.Code, recPathBlocked.Body.String())

	srv.SetAllowLocalIGPathInstall(true)
	reqPathAllowed := httptest.NewRequest(http.MethodPost, "/fhir/r4/$install-ig", strings.NewReader(pathPayload))
	reqPathAllowed.Header.Set("Content-Type", "application/fhir+json")
	recPathAllowed := httptest.NewRecorder()
	h.ServeHTTP(recPathAllowed, reqPathAllowed)
	require.Equal(t, http.StatusOK, recPathAllowed.Code, recPathAllowed.Body.String())

	// 5. Reject R5 package with 422 Unprocessable Entity, empty body with 400, wrong method with 405
	r5TGZ := buildTestTGZBytes(t, map[string]string{
		"package/package.json": `{"name":"hl7.fhir.uv.r5only","version":"1.0.0","fhirVersions":["5.0.0"]}`,
	})
	reqR5 := httptest.NewRequest(http.MethodPost, "/fhir/r4/$install-ig", bytes.NewReader(r5TGZ))
	reqR5.Header.Set("Content-Type", "application/gzip")
	recR5 := httptest.NewRecorder()
	h.ServeHTTP(recR5, reqR5)
	assert.Equal(t, http.StatusUnprocessableEntity, recR5.Code, recR5.Body.String())

	reqEmpty := httptest.NewRequest(http.MethodPost, "/fhir/r4/$install-ig", strings.NewReader("   "))
	recEmpty := httptest.NewRecorder()
	h.ServeHTTP(recEmpty, reqEmpty)
	assert.Equal(t, http.StatusBadRequest, recEmpty.Code)

	reqWrongRT := httptest.NewRequest(http.MethodPost, "/fhir/r4/$install-ig", strings.NewReader(`{"resourceType":"Patient"}`))
	reqWrongRT.Header.Set("Content-Type", "application/fhir+json")
	recWrongRT := httptest.NewRecorder()
	h.ServeHTTP(recWrongRT, reqWrongRT)
	assert.Equal(t, http.StatusBadRequest, recWrongRT.Code)

	reqEmptyParams := httptest.NewRequest(http.MethodPost, "/fhir/r4/$install-ig", strings.NewReader(`{"resourceType":"Parameters"}`))
	reqEmptyParams.Header.Set("Content-Type", "application/fhir+json")
	recEmptyParams := httptest.NewRecorder()
	h.ServeHTTP(recEmptyParams, reqEmptyParams)
	assert.Equal(t, http.StatusBadRequest, recEmptyParams.Code)

	reqMethod := httptest.NewRequest(http.MethodPost, "/fhir/r4/$igs", nil)
	recMethod := httptest.NewRecorder()
	h.ServeHTTP(recMethod, reqMethod)
	assert.Equal(t, http.StatusMethodNotAllowed, recMethod.Code)

	reqGetInstall := httptest.NewRequest(http.MethodGet, "/fhir/r4/$install-ig", nil)
	recGetInstall := httptest.NewRecorder()
	h.ServeHTTP(recGetInstall, reqGetInstall)
	assert.Equal(t, http.StatusMethodNotAllowed, recGetInstall.Code)
}

func TestIGPackageInstall_SMARTRBACAndAuditEvents(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(nil, logger)
	server.RegisterDefaultResourceTypes(srv)

	mockAuth, err := auth.NewMockTokenValidator("https://auth.example.org", "https://fhir.example.org")
	require.NoError(t, err)
	srv.SetTokenValidator(mockAuth)

	memAudit := audit.NewMemoryRecorder()
	srv.SetAuditRecorder(memAudit)

	h := srv.Handler()
	tgzBytes := buildTestTGZBytes(t, map[string]string{
		"package/package.json": `{"name":"sec.test.ig","version":"1.0.0","fhirVersions":["4.0.1"]}`,
	})

	issueTok := func(sub string, scopes []string, patientID, tenantID string) string {
		var extra map[string]any
		if tenantID != "" {
			extra = map[string]any{"tenant_id": tenantID}
		}
		tok, err := mockAuth.IssueToken(sub, strings.Join(scopes, " "), patientID, time.Hour, extra)
		require.NoError(t, err)
		return tok
	}

	patientTok := issueTok("pat-user", []string{"patient/*.cruds"}, "pat-1", "default")
	clinicianPatientReadTok := issueTok("doc-user", []string{"user/Patient.rs"}, "", "default")
	igAdminTok := issueTok("ig-admin", []string{"user/ImplementationGuide.cruds"}, "", "default")
	otherTenantAdminTok := issueTok("tenant2-admin", []string{"system/*.cruds"}, "", "tenant-b")

	// 1. Unauthenticated -> 401 Unauthorized
	reqUnauth := httptest.NewRequest(http.MethodPost, "/fhir/r4/$install-ig", bytes.NewReader(tgzBytes))
	recUnauth := httptest.NewRecorder()
	h.ServeHTTP(recUnauth, reqUnauth)
	assert.Equal(t, http.StatusUnauthorized, recUnauth.Code)

	// 2. Patient-scoped token -> 403 Forbidden on both system and tenant IG endpoints
	for _, target := range []struct {
		method string
		url    string
		tok    string
	}{
		{http.MethodPost, "/fhir/r4/$install-ig", patientTok},
		{http.MethodPost, "/fhir/r4/default/$install-ig", patientTok},
		{http.MethodGet, "/fhir/r4/$igs", patientTok},
		{http.MethodGet, "/fhir/r4/default/$igs", patientTok},
		{http.MethodPost, "/fhir/r4/$install-ig", clinicianPatientReadTok},
		{http.MethodGet, "/fhir/r4/default/$igs", clinicianPatientReadTok},
		{http.MethodPost, "/fhir/r4/$install-ig", otherTenantAdminTok},
	} {
		var body io.Reader
		if target.method == http.MethodPost {
			body = bytes.NewReader(tgzBytes)
		}
		req := httptest.NewRequest(target.method, target.url, body)
		req.Header.Set("Authorization", "Bearer "+target.tok)
		if target.method == http.MethodPost {
			req.Header.Set("Content-Type", "application/gzip")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s should be forbidden", target.method, target.url)
	}

	// 3. Authorized user/ImplementationGuide.cruds token -> 200 OK on both POST /$install-ig and GET /$igs
	reqOK := httptest.NewRequest(http.MethodPost, "/fhir/r4/$install-ig", bytes.NewReader(tgzBytes))
	reqOK.Header.Set("Authorization", "Bearer "+igAdminTok)
	reqOK.Header.Set("Content-Type", "application/gzip")
	recOK := httptest.NewRecorder()
	h.ServeHTTP(recOK, reqOK)
	require.Equal(t, http.StatusOK, recOK.Code, recOK.Body.String())

	reqListOK := httptest.NewRequest(http.MethodGet, "/fhir/r4/default/$igs", nil)
	reqListOK.Header.Set("Authorization", "Bearer "+igAdminTok)
	recListOK := httptest.NewRecorder()
	h.ServeHTTP(recListOK, reqListOK)
	require.Equal(t, http.StatusOK, recListOK.Code, recListOK.Body.String())

	// 4. Verify AuditEvent records were emitted for both $install-ig and $igs!
	installAudits, totalInstall, err := memAudit.Query(context.Background(), store.AuditQueryParams{
		TenantID:    "default",
		SubtypeCode: "install-ig",
		Outcome:     audit.OutcomeSuccess,
	})
	require.NoError(t, err)
	require.GreaterOrEqual(t, totalInstall, 1)
	assert.Equal(t, "ig-admin", installAudits[0].AgentSubject)
	assert.Equal(t, "ImplementationGuide", installAudits[0].EntityType)

	listAudits, totalList, err := memAudit.Query(context.Background(), store.AuditQueryParams{
		TenantID:    "default",
		SubtypeCode: "igs",
		Outcome:     audit.OutcomeSuccess,
	})
	require.NoError(t, err)
	require.GreaterOrEqual(t, totalList, 1)
	assert.Equal(t, "ig-admin", listAudits[0].AgentSubject)
}

func mustJSONQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
