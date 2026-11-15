package server_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	patpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto"

	"github.com/flint-fhir/flint/pkg/fhirxml"
	"github.com/flint-fhir/flint/pkg/validation"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/postgres"
)

func TestFHIRXMLWire_EndToEndCRUD_Search_Bundle_Validate_Convert_And_Security(t *testing.T) {
	db := openMemPostgres(t)
	defer db.Close()

	pgStore := postgres.New(db, postgres.WithTenantIsolation(postgres.TenantIsolationShared))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv := server.New(pgStore, logger)
	server.RegisterDefaultResourceTypes(srv)
	srv.SetValidator(validation.NewEngine(validation.DefaultOptions()))

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := ts.Client()
	baseURL := ts.URL + "/fhir/r4/xmltest"

	// 1. CapabilityStatement via ?_format=xml advertises XML & FHIRPath XML patch formats
	respMeta, err := client.Get(baseURL + "/metadata?_format=xml")
	require.NoError(t, err)
	metaBody, _ := io.ReadAll(respMeta.Body)
	respMeta.Body.Close()
	require.Equal(t, http.StatusOK, respMeta.StatusCode)
	assert.Equal(t, "application/fhir+xml; charset=utf-8", respMeta.Header.Get("Content-Type"))
	assert.Equal(t, "hl7.fhir.r4.xml", respMeta.Header.Get("X-Flint-Wire-Format"))
	assert.Contains(t, string(metaBody), `<CapabilityStatement xmlns="http://hl7.org/fhir">`)
	assert.Contains(t, string(metaBody), `<format value="application/fhir+xml"/>`)
	assert.Contains(t, string(metaBody), `<patchFormat value="application/fhir+xml"/>`)

	capMap, err := fhirxml.UnmarshalFHIRMap(metaBody)
	require.NoError(t, err)
	assert.Equal(t, "CapabilityStatement", capMap["resourceType"])

	// Browser Accept header (text/html higher q than application/xml;q=0.9) defaults cleanly to JSON
	reqBrowser, _ := http.NewRequest(http.MethodGet, baseURL+"/metadata", nil)
	reqBrowser.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	respBrowser, err := client.Do(reqBrowser)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, respBrowser.Body)
	respBrowser.Body.Close()
	assert.Equal(t, http.StatusOK, respBrowser.StatusCode)
	assert.Contains(t, respBrowser.Header.Get("Content-Type"), "application/fhir+json")

	// 2. POST /Patient with Content-Type: application/fhir+xml (and no Accept -> echoes XML response)
	patientXML := `<?xml version="1.0" encoding="UTF-8"?>
<Patient xmlns="http://hl7.org/fhir">
  <text>
    <status value="generated"/>
    <div xmlns="http://www.w3.org/1999/xhtml"><p>Patient <b>Alice XML</b></p></div>
  </text>
  <contained>
    <Organization>
      <id value="org-1"/>
      <active value="true"/>
      <name value="XML Clinic"/>
    </Organization>
  </contained>
  <extension url="http://hl7.org/fhir/StructureDefinition/patient-mothersMaidenName">
    <valueString value=" Hopper"/>
  </extension>
  <identifier>
    <system value="http://hospital.example.org/mrn"/>
    <value value="MRN-XML-001"/>
  </identifier>
  <active value="true"/>
  <name id="hn-1">
    <use value="official"/>
    <family value="Xmlson"/>
    <given value="Alice"/>
    <given id="g-mid" value="Marie"/>
  </name>
  <gender value="female"/>
  <birthDate value="1985-04-12">
    <extension url="http://hl7.org/fhir/StructureDefinition/patient-birthTime">
      <valueDateTime value="1985-04-12T08:15:00Z"/>
    </extension>
  </birthDate>
  <managingOrganization>
    <reference value="#org-1"/>
    <display value="XML Clinic"/>
  </managingOrganization>
</Patient>`

	reqCreate, _ := http.NewRequest(http.MethodPost, baseURL+"/Patient", strings.NewReader(patientXML))
	reqCreate.Header.Set("Content-Type", "application/fhir+xml; charset=utf-8")
	respCreate, err := client.Do(reqCreate)
	require.NoError(t, err)
	createBody, _ := io.ReadAll(respCreate.Body)
	respCreate.Body.Close()
	require.Equal(t, http.StatusCreated, respCreate.StatusCode, string(createBody))
	assert.Equal(t, "application/fhir+xml; charset=utf-8", respCreate.Header.Get("Content-Type"))
	assert.Equal(t, `W/"1"`, respCreate.Header.Get("ETag"))

	createdPat, err := fhirxml.UnmarshalFHIRMap(createBody)
	require.NoError(t, err)
	patID, _ := createdPat["id"].(string)
	require.NotEmpty(t, patID)
	assert.Equal(t, "female", createdPat["gender"])

	// 3. Tri-wire Read: JSON, XML (Accept quality weights + _summary + HEAD + 304), and Proto3
	reqReadJSON, _ := http.NewRequest(http.MethodGet, baseURL+"/Patient/"+patID, nil)
	reqReadJSON.Header.Set("Accept", "application/fhir+json")
	respReadJSON, err := client.Do(reqReadJSON)
	require.NoError(t, err)
	readJSONBytes, _ := io.ReadAll(respReadJSON.Body)
	respReadJSON.Body.Close()
	require.Equal(t, http.StatusOK, respReadJSON.StatusCode)
	assert.Contains(t, respReadJSON.Header.Get("Content-Type"), "application/fhir+json")
	var jsonPatMap map[string]any
	require.NoError(t, json.Unmarshal(readJSONBytes, &jsonPatMap))
	assert.Equal(t, "1985-04-12", jsonPatMap["birthDate"])
	assert.NotNil(t, jsonPatMap["_birthDate"])

	reqReadXMLQ, _ := http.NewRequest(http.MethodGet, baseURL+"/Patient/"+patID, nil)
	reqReadXMLQ.Header.Set("Accept", "application/fhir+json;q=0.5, application/fhir+xml;q=0.9")
	respReadXMLQ, err := client.Do(reqReadXMLQ)
	require.NoError(t, err)
	readXMLBytes, _ := io.ReadAll(respReadXMLQ.Body)
	respReadXMLQ.Body.Close()
	require.Equal(t, http.StatusOK, respReadXMLQ.StatusCode)
	assert.Equal(t, "application/fhir+xml; charset=utf-8", respReadXMLQ.Header.Get("Content-Type"))
	assert.Contains(t, string(readXMLBytes), `<div xmlns="http://www.w3.org/1999/xhtml"><p>Patient <b>Alice XML</b></p></div>`)
	assert.Contains(t, string(readXMLBytes), `<contained><Organization><id value="org-1"/>`)

	// _summary=true in XML
	respSummaryXML, err := client.Get(baseURL + "/Patient/" + patID + "?_summary=true&_format=application/fhir+xml")
	require.NoError(t, err)
	sumXMLBytes, _ := io.ReadAll(respSummaryXML.Body)
	respSummaryXML.Body.Close()
	require.Equal(t, http.StatusOK, respSummaryXML.StatusCode)
	assert.Equal(t, "application/fhir+xml; charset=utf-8", respSummaryXML.Header.Get("Content-Type"))
	assert.Contains(t, string(sumXMLBytes), `SUBSETTED`)
	assert.NotContains(t, string(sumXMLBytes), `XML Clinic</p></div>`)

	// HEAD & Conditional Read 304 Not Modified with Accept: application/fhir+xml
	reqHead, _ := http.NewRequest(http.MethodHead, baseURL+"/Patient/"+patID, nil)
	reqHead.Header.Set("Accept", "application/fhir+xml")
	respHead, err := client.Do(reqHead)
	require.NoError(t, err)
	headBytes, _ := io.ReadAll(respHead.Body)
	respHead.Body.Close()
	assert.Equal(t, http.StatusOK, respHead.StatusCode)
	assert.Equal(t, "application/fhir+xml; charset=utf-8", respHead.Header.Get("Content-Type"))
	assert.Empty(t, headBytes)

	req304, _ := http.NewRequest(http.MethodGet, baseURL+"/Patient/"+patID, nil)
	req304.Header.Set("Accept", "application/fhir+xml")
	req304.Header.Set("If-None-Match", `W/"1"`)
	resp304, err := client.Do(req304)
	require.NoError(t, err)
	resp304.Body.Close()
	assert.Equal(t, http.StatusNotModified, resp304.StatusCode)

	// Binary Proto3 read of the XML-created Patient
	reqReadProto, _ := http.NewRequest(http.MethodGet, baseURL+"/Patient/"+patID, nil)
	reqReadProto.Header.Set("Accept", "application/fhir+protobuf")
	respReadProto, err := client.Do(reqReadProto)
	require.NoError(t, err)
	protoBytes, _ := io.ReadAll(respReadProto.Body)
	respReadProto.Body.Close()
	require.Equal(t, http.StatusOK, respReadProto.StatusCode)
	var patProto patpb.Patient
	require.NoError(t, proto.Unmarshal(protoBytes, &patProto))
	require.Len(t, patProto.GetName(), 1)
	assert.Equal(t, "Xmlson", patProto.GetName()[0].GetFamily().GetValue())

	// 4. PUT Update & Conditional Update in XML (application/xml) + Prefer: return=OperationOutcome
	updatedXML := strings.Replace(patientXML, `<gender value="female"/>`, `<gender value="other"/>`, 1)
	updatedXML = strings.Replace(updatedXML, "<Patient xmlns=\"http://hl7.org/fhir\">", "<Patient xmlns=\"http://hl7.org/fhir\"><id value=\""+patID+"\"/>", 1)

	reqPut, _ := http.NewRequest(http.MethodPut, baseURL+"/Patient/"+patID, strings.NewReader(updatedXML))
	reqPut.Header.Set("Content-Type", "application/xml")
	reqPut.Header.Set("Accept", "application/xml")
	reqPut.Header.Set("Prefer", "return=OperationOutcome")
	respPut, err := client.Do(reqPut)
	require.NoError(t, err)
	putBody, _ := io.ReadAll(respPut.Body)
	respPut.Body.Close()
	require.Equal(t, http.StatusOK, respPut.StatusCode, string(putBody))
	assert.Equal(t, "application/fhir+xml; charset=utf-8", respPut.Header.Get("Content-Type"))
	assert.Contains(t, string(putBody), `<OperationOutcome xmlns="http://hl7.org/fhir">`)

	// Conditional Update via ?identifier=
	condXML := strings.Replace(updatedXML, `<gender value="other"/>`, `<gender value="unknown"/>`, 1)
	reqCondPut, _ := http.NewRequest(http.MethodPut, baseURL+"/Patient?identifier=http://hospital.example.org/mrn|MRN-XML-001", strings.NewReader(condXML))
	reqCondPut.Header.Set("Content-Type", "text/xml")
	reqCondPut.Header.Set("Accept", "text/xml")
	respCondPut, err := client.Do(reqCondPut)
	require.NoError(t, err)
	condPutBody, _ := io.ReadAll(respCondPut.Body)
	respCondPut.Body.Close()
	require.Equal(t, http.StatusOK, respCondPut.StatusCode, string(condPutBody))
	assert.Contains(t, string(condPutBody), `<gender value="unknown"/>`)

	// 5. PATCH in XML using HL7 FHIRPath Parameters Patch
	fhirPatchXML := `<Parameters xmlns="http://hl7.org/fhir">
  <parameter>
    <name value="operation"/>
    <part>
      <name value="type"/>
      <valueCode value="replace"/>
    </part>
    <part>
      <name value="path"/>
      <valueString value="Patient.gender"/>
    </part>
    <part>
      <name value="value"/>
      <valueCode value="female"/>
    </part>
  </parameter>
</Parameters>`
	reqPatch, _ := http.NewRequest(http.MethodPatch, baseURL+"/Patient/"+patID, strings.NewReader(fhirPatchXML))
	reqPatch.Header.Set("Content-Type", "application/fhir+xml")
	reqPatch.Header.Set("Accept", "application/fhir+xml")
	respPatch, err := client.Do(reqPatch)
	require.NoError(t, err)
	patchBody, _ := io.ReadAll(respPatch.Body)
	respPatch.Body.Close()
	require.Equal(t, http.StatusOK, respPatch.StatusCode, string(patchBody))
	assert.Contains(t, string(patchBody), `<gender value="female"/>`)

	// 6. VRead (version 1) & Instance _history Bundle in XML
	respVRead, err := client.Get(baseURL + "/Patient/" + patID + "/_history/1?_format=xml")
	require.NoError(t, err)
	vreadBody, _ := io.ReadAll(respVRead.Body)
	respVRead.Body.Close()
	require.Equal(t, http.StatusOK, respVRead.StatusCode)
	assert.Equal(t, "application/fhir+xml; charset=utf-8", respVRead.Header.Get("Content-Type"))
	assert.Contains(t, string(vreadBody), `<versionId value="1"/>`)

	respHist, err := client.Get(baseURL + "/Patient/" + patID + "/_history?_format=xml")
	require.NoError(t, err)
	histBody, _ := io.ReadAll(respHist.Body)
	respHist.Body.Close()
	require.Equal(t, http.StatusOK, respHist.StatusCode)
	histMap, err := fhirxml.UnmarshalFHIRMap(histBody)
	require.NoError(t, err)
	assert.Equal(t, "Bundle", histMap["resourceType"])
	assert.Equal(t, "history", histMap["type"])

	// 7. Search Bundle in XML
	respSearch, err := client.Get(baseURL + "/Patient?family=Xmlson&_format=xml")
	require.NoError(t, err)
	searchBody, _ := io.ReadAll(respSearch.Body)
	respSearch.Body.Close()
	require.Equal(t, http.StatusOK, respSearch.StatusCode)
	searchMap, err := fhirxml.UnmarshalFHIRMap(searchBody)
	require.NoError(t, err)
	assert.Equal(t, "Bundle", searchMap["resourceType"])
	assert.Equal(t, "searchset", searchMap["type"])
	assert.Equal(t, float64(1), searchMap["total"])

	// 8. Transaction Bundle in XML (POST Patient + POST Observation with urn:uuid reference + GET Observation)
	txBundleXML := `<?xml version="1.0" encoding="UTF-8"?>
<Bundle xmlns="http://hl7.org/fhir">
  <type value="transaction"/>
  <entry>
    <fullUrl value="urn:uuid:11111111-2222-3333-4444-555555555555"/>
    <resource>
      <Patient>
        <active value="true"/>
        <name>
          <family value="BundleXml"/>
          <given value="Bob"/>
        </name>
        <gender value="male"/>
      </Patient>
    </resource>
    <request>
      <method value="POST"/>
      <url value="Patient"/>
    </request>
  </entry>
  <entry>
    <fullUrl value="urn:uuid:66666666-7777-8888-9999-000000000000"/>
    <resource>
      <Observation>
        <status value="final"/>
        <code>
          <coding>
            <system value="http://loinc.org"/>
            <code value="8310-5"/>
            <display value="Body temperature"/>
          </coding>
        </code>
        <subject>
          <reference value="urn:uuid:11111111-2222-3333-4444-555555555555"/>
        </subject>
        <valueQuantity>
          <value value="98.60"/>
          <unit value="degF"/>
          <system value="http://unitsofmeasure.org"/>
          <code value="[degF]"/>
        </valueQuantity>
      </Observation>
    </resource>
    <request>
      <method value="POST"/>
      <url value="Observation"/>
    </request>
  </entry>
</Bundle>`
	reqTx, _ := http.NewRequest(http.MethodPost, baseURL, strings.NewReader(txBundleXML))
	reqTx.Header.Set("Content-Type", "application/fhir+xml")
	reqTx.Header.Set("Accept", "application/fhir+xml")
	respTx, err := client.Do(reqTx)
	require.NoError(t, err)
	txRespBody, _ := io.ReadAll(respTx.Body)
	respTx.Body.Close()
	require.Equal(t, http.StatusOK, respTx.StatusCode, string(txRespBody))
	assert.Equal(t, "application/fhir+xml; charset=utf-8", respTx.Header.Get("Content-Type"))
	txRespMap, err := fhirxml.UnmarshalFHIRMap(txRespBody)
	require.NoError(t, err)
	assert.Equal(t, "transaction-response", txRespMap["type"])
	require.Len(t, txRespMap["entry"].([]any), 2)

	// 9. $validate with XML resource & XML Parameters + invalid XML StructureDefinition error
	reqValOK, _ := http.NewRequest(http.MethodPost, baseURL+"/Patient/$validate", strings.NewReader(patientXML))
	reqValOK.Header.Set("Content-Type", "application/fhir+xml")
	reqValOK.Header.Set("Accept", "application/fhir+xml")
	respValOK, err := client.Do(reqValOK)
	require.NoError(t, err)
	valOKBody, _ := io.ReadAll(respValOK.Body)
	respValOK.Body.Close()
	require.Equal(t, http.StatusOK, respValOK.StatusCode, string(valOKBody))
	assert.Contains(t, string(valOKBody), `<OperationOutcome xmlns="http://hl7.org/fhir">`)

	invalidObsXML := `<Observation xmlns="http://hl7.org/fhir"><id value="missing-required-fields"/></Observation>`
	reqValBad, _ := http.NewRequest(http.MethodPost, baseURL+"/Observation/$validate", strings.NewReader(invalidObsXML))
	reqValBad.Header.Set("Content-Type", "application/fhir+xml")
	reqValBad.Header.Set("Accept", "application/fhir+xml")
	respValBad, err := client.Do(reqValBad)
	require.NoError(t, err)
	valBadBody, _ := io.ReadAll(respValBad.Body)
	respValBad.Body.Close()
	assert.Equal(t, http.StatusBadRequest, respValBad.StatusCode)
	assert.Equal(t, "application/fhir+xml; charset=utf-8", respValBad.Header.Get("Content-Type"))
	assert.Contains(t, string(valBadBody), `<OperationOutcome xmlns="http://hl7.org/fhir">`)
	assert.Contains(t, string(valBadBody), `<severity value="error"/>`)

	// 10. $convert: XML -> JSON, JSON -> XML (?format=xml), Proto3 -> XML (Accept: application/fhir+xml)
	reqConvXMLToJSON, _ := http.NewRequest(http.MethodPost, baseURL+"/Patient/$convert", strings.NewReader(patientXML))
	reqConvXMLToJSON.Header.Set("Content-Type", "application/fhir+xml")
	respConvJSON, err := client.Do(reqConvXMLToJSON)
	require.NoError(t, err)
	convJSONBytes, _ := io.ReadAll(respConvJSON.Body)
	respConvJSON.Body.Close()
	require.Equal(t, http.StatusOK, respConvJSON.StatusCode)
	assert.Contains(t, respConvJSON.Header.Get("Content-Type"), "application/fhir+json")

	reqConvJSONToXML, _ := http.NewRequest(http.MethodPost, baseURL+"/Patient/$convert?format=xml", bytes.NewReader(convJSONBytes))
	reqConvJSONToXML.Header.Set("Content-Type", "application/fhir+json")
	respConvXML, err := client.Do(reqConvJSONToXML)
	require.NoError(t, err)
	convXMLBytes, _ := io.ReadAll(respConvXML.Body)
	respConvXML.Body.Close()
	require.Equal(t, http.StatusOK, respConvXML.StatusCode)
	assert.Equal(t, "application/fhir+xml; charset=utf-8", respConvXML.Header.Get("Content-Type"))
	assert.Contains(t, string(convXMLBytes), `<family value="Xmlson"/>`)

	reqConvProtoToXML, _ := http.NewRequest(http.MethodPost, baseURL+"/Patient/$convert", bytes.NewReader(protoBytes))
	reqConvProtoToXML.Header.Set("Content-Type", "application/fhir+protobuf")
	reqConvProtoToXML.Header.Set("Accept", "application/fhir+xml")
	respConvProtoXML, err := client.Do(reqConvProtoToXML)
	require.NoError(t, err)
	convProtoXMLBytes, _ := io.ReadAll(respConvProtoXML.Body)
	respConvProtoXML.Body.Close()
	require.Equal(t, http.StatusOK, respConvProtoXML.StatusCode)
	assert.Equal(t, "application/fhir+xml; charset=utf-8", respConvProtoXML.Header.Get("Content-Type"))
	assert.Contains(t, string(convProtoXMLBytes), `<family value="Xmlson"/>`)

	// 11. Terminology ValueSet/$expand, Patient/$match, and $meta-add in XML
	expandParamsXML := `<Parameters xmlns="http://hl7.org/fhir">
  <parameter>
    <name value="url"/>
    <valueUri value="http://hl7.org/fhir/ValueSet/administrative-gender"/>
  </parameter>
</Parameters>`
	reqExpand, _ := http.NewRequest(http.MethodPost, baseURL+"/ValueSet/$expand", strings.NewReader(expandParamsXML))
	reqExpand.Header.Set("Content-Type", "application/fhir+xml")
	reqExpand.Header.Set("Accept", "application/fhir+xml")
	respExpand, err := client.Do(reqExpand)
	require.NoError(t, err)
	expandBody, _ := io.ReadAll(respExpand.Body)
	respExpand.Body.Close()
	require.Equal(t, http.StatusOK, respExpand.StatusCode, string(expandBody))
	assert.Contains(t, string(expandBody), `<ValueSet xmlns="http://hl7.org/fhir">`)

	matchXML := `<Parameters xmlns="http://hl7.org/fhir">
  <parameter>
    <name value="resource"/>
    <resource>
      <Patient>
        <name><family value="Xmlson"/><given value="Alice"/></name>
        <birthDate value="1985-04-12"/>
      </Patient>
    </resource>
  </parameter>
</Parameters>`
	reqMatch, _ := http.NewRequest(http.MethodPost, baseURL+"/Patient/$match", strings.NewReader(matchXML))
	reqMatch.Header.Set("Content-Type", "application/fhir+xml")
	reqMatch.Header.Set("Accept", "application/fhir+xml")
	respMatch, err := client.Do(reqMatch)
	require.NoError(t, err)
	matchBody, _ := io.ReadAll(respMatch.Body)
	respMatch.Body.Close()
	require.Equal(t, http.StatusOK, respMatch.StatusCode, string(matchBody))
	assert.Contains(t, string(matchBody), `<Bundle xmlns="http://hl7.org/fhir">`)

	metaAddXML := `<Parameters xmlns="http://hl7.org/fhir">
  <parameter>
    <name value="meta"/>
    <valueMeta>
      <tag>
        <system value="http://example.org/tags"/>
        <code value="xml-verified"/>
      </tag>
    </valueMeta>
  </parameter>
</Parameters>`
	reqMetaAdd, _ := http.NewRequest(http.MethodPost, baseURL+"/Patient/"+patID+"/$meta-add", strings.NewReader(metaAddXML))
	reqMetaAdd.Header.Set("Content-Type", "application/fhir+xml")
	reqMetaAdd.Header.Set("Accept", "application/fhir+xml")
	respMetaAdd, err := client.Do(reqMetaAdd)
	require.NoError(t, err)
	metaAddBody, _ := io.ReadAll(respMetaAdd.Body)
	respMetaAdd.Body.Close()
	require.Equal(t, http.StatusOK, respMetaAdd.StatusCode, string(metaAddBody))
	assert.Contains(t, string(metaAddBody), `xml-verified`)

	// 12. Binary resource: raw bytes on Accept: text/plain vs FHIR XML wrapper on ?_format=xml
	rawText := "Hello Clinical Note"
	binXML := `<Binary xmlns="http://hl7.org/fhir">
  <contentType value="text/plain"/>
  <data value="` + base64.StdEncoding.EncodeToString([]byte(rawText)) + `"/>
</Binary>`
	reqBinCreate, _ := http.NewRequest(http.MethodPost, baseURL+"/Binary", strings.NewReader(binXML))
	reqBinCreate.Header.Set("Content-Type", "application/fhir+xml")
	respBinCreate, err := client.Do(reqBinCreate)
	require.NoError(t, err)
	binCreateBody, _ := io.ReadAll(respBinCreate.Body)
	respBinCreate.Body.Close()
	require.Equal(t, http.StatusCreated, respBinCreate.StatusCode)
	binMap, err := fhirxml.UnmarshalFHIRMap(binCreateBody)
	require.NoError(t, err)
	binID := binMap["id"].(string)

	reqBinRaw, _ := http.NewRequest(http.MethodGet, baseURL+"/Binary/"+binID, nil)
	reqBinRaw.Header.Set("Accept", "text/plain")
	respBinRaw, err := client.Do(reqBinRaw)
	require.NoError(t, err)
	binRawBytes, _ := io.ReadAll(respBinRaw.Body)
	respBinRaw.Body.Close()
	assert.Equal(t, http.StatusOK, respBinRaw.StatusCode)
	assert.Equal(t, "text/plain", respBinRaw.Header.Get("Content-Type"))
	assert.Equal(t, rawText, string(binRawBytes))

	respBinWrap, err := client.Get(baseURL + "/Binary/" + binID + "?_format=xml")
	require.NoError(t, err)
	binWrapBytes, _ := io.ReadAll(respBinWrap.Body)
	respBinWrap.Body.Close()
	assert.Equal(t, http.StatusOK, respBinWrap.StatusCode)
	assert.Equal(t, "application/fhir+xml; charset=utf-8", respBinWrap.Header.Get("Content-Type"))
	assert.Contains(t, string(binWrapBytes), `<Binary xmlns="http://hl7.org/fhir">`)

	// 13. DELETE with Prefer: return=OperationOutcome in XML + 410 Gone in XML
	reqDel, _ := http.NewRequest(http.MethodDelete, baseURL+"/Patient/"+patID, nil)
	reqDel.Header.Set("Accept", "application/fhir+xml")
	reqDel.Header.Set("Prefer", "return=OperationOutcome")
	respDel, err := client.Do(reqDel)
	require.NoError(t, err)
	delBody, _ := io.ReadAll(respDel.Body)
	respDel.Body.Close()
	assert.Equal(t, http.StatusOK, respDel.StatusCode)
	assert.Equal(t, "application/fhir+xml; charset=utf-8", respDel.Header.Get("Content-Type"))
	assert.Contains(t, string(delBody), `<OperationOutcome xmlns="http://hl7.org/fhir">`)

	respGone, err := client.Get(baseURL + "/Patient/" + patID + "?_format=xml")
	require.NoError(t, err)
	goneBody, _ := io.ReadAll(respGone.Body)
	respGone.Body.Close()
	assert.Equal(t, http.StatusGone, respGone.StatusCode)
	assert.Equal(t, "application/fhir+xml; charset=utf-8", respGone.Header.Get("Content-Type"))
	assert.Contains(t, string(goneBody), `<code value="deleted"/>`)

	// 14. Security & Malformed XML Rejection: XXE <!DOCTYPE, resourceType mismatch, 404 Not Found in XML
	xxePayload := `<?xml version="1.0"?><!DOCTYPE foo [<!ENTITY xxe SYSTEM "file:///etc/passwd">]><Patient xmlns="http://hl7.org/fhir"><id value="&xxe;"/></Patient>`
	reqXXE, _ := http.NewRequest(http.MethodPost, baseURL+"/Patient", strings.NewReader(xxePayload))
	reqXXE.Header.Set("Content-Type", "application/fhir+xml")
	reqXXE.Header.Set("Accept", "application/fhir+xml")
	respXXE, err := client.Do(reqXXE)
	require.NoError(t, err)
	xxeRespBody, _ := io.ReadAll(respXXE.Body)
	respXXE.Body.Close()
	assert.Equal(t, http.StatusBadRequest, respXXE.StatusCode)
	assert.Equal(t, "application/fhir+xml; charset=utf-8", respXXE.Header.Get("Content-Type"))
	assert.Contains(t, string(xxeRespBody), `<OperationOutcome xmlns="http://hl7.org/fhir">`)

	mismatchXML := `<Observation xmlns="http://hl7.org/fhir"><status value="final"/></Observation>`
	reqMismatch, _ := http.NewRequest(http.MethodPost, baseURL+"/Patient", strings.NewReader(mismatchXML))
	reqMismatch.Header.Set("Content-Type", "application/fhir+xml")
	reqMismatch.Header.Set("Accept", "application/fhir+xml")
	respMismatch, err := client.Do(reqMismatch)
	require.NoError(t, err)
	mismatchBody, _ := io.ReadAll(respMismatch.Body)
	respMismatch.Body.Close()
	assert.Equal(t, http.StatusBadRequest, respMismatch.StatusCode)
	assert.Contains(t, string(mismatchBody), `resourceType mismatch`)

	resp404, err := client.Get(baseURL + "/Patient/does-not-exist-999?_format=xml")
	require.NoError(t, err)
	notFoundBody, _ := io.ReadAll(resp404.Body)
	resp404.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp404.StatusCode)
	assert.Equal(t, "application/fhir+xml; charset=utf-8", resp404.Header.Get("Content-Type"))
	assert.Contains(t, string(notFoundBody), `<code value="not-found"/>`)

	// 15. Additional Accept / q-factor / wildcard / $closure / $validate Parameters coverage
	reqWildcardPost, _ := http.NewRequest(http.MethodPost, baseURL+"/Patient", strings.NewReader(patientXML))
	reqWildcardPost.Header.Set("Content-Type", "application/fhir+xml")
	reqWildcardPost.Header.Set("Accept", "*/*")
	respWildcard, err := client.Do(reqWildcardPost)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, respWildcard.Body)
	respWildcard.Body.Close()
	assert.Equal(t, http.StatusCreated, respWildcard.StatusCode)
	assert.Equal(t, "application/fhir+xml; charset=utf-8", respWildcard.Header.Get("Content-Type"))

	reqZeroQ, _ := http.NewRequest(http.MethodGet, baseURL+"/metadata", nil)
	reqZeroQ.Header.Set("Accept", "application/fhir+xml;q=0, application/fhir+json;q=0.8")
	respZeroQ, err := client.Do(reqZeroQ)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, respZeroQ.Body)
	respZeroQ.Body.Close()
	assert.Contains(t, respZeroQ.Header.Get("Content-Type"), "application/fhir+json")

	reqHead404, _ := http.NewRequest(http.MethodHead, baseURL+"/Patient/missing-head-xml", nil)
	reqHead404.Header.Set("Accept", "application/fhir+xml")
	respHead404, err := client.Do(reqHead404)
	require.NoError(t, err)
	respHead404.Body.Close()
	assert.Equal(t, http.StatusNotFound, respHead404.StatusCode)
	assert.Equal(t, "application/fhir+xml; charset=utf-8", respHead404.Header.Get("Content-Type"))

	closureXML := `<Parameters xmlns="http://hl7.org/fhir">
  <parameter>
    <name value="name"/>
    <valueString value="xml-closure-table"/>
  </parameter>
</Parameters>`
	reqClosure, _ := http.NewRequest(http.MethodPost, baseURL+"/$closure", strings.NewReader(closureXML))
	reqClosure.Header.Set("Content-Type", "application/fhir+xml")
	reqClosure.Header.Set("Accept", "application/fhir+xml")
	respClosure, err := client.Do(reqClosure)
	require.NoError(t, err)
	closureBody, _ := io.ReadAll(respClosure.Body)
	respClosure.Body.Close()
	assert.Equal(t, http.StatusOK, respClosure.StatusCode)
	assert.Contains(t, string(closureBody), `<ConceptMap xmlns="http://hl7.org/fhir">`)
}
