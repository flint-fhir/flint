package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/pkg/fhirutil"
	"github.com/flint-fhir/flint/store/postgres"
)

func canonicalR4ResourceType(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	norm := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(raw), "-", ""), "_", "")
	for _, rt := range fhirutil.AllR4ResourceTypes() {
		if strings.EqualFold(rt, raw) || strings.ToLower(rt) == norm {
			return rt
		}
	}
	return raw
}

type testScriptInteractionCapture struct {
	reqMethod  string
	reqURL     string
	reqHeaders http.Header
	reqBody    map[string]any
	statusCode int
	headers    http.Header
	rawBody    []byte
	bodyMap    map[string]any
	resType    string
	resID      string
}

// handleTestScriptEvaluate handles GET/POST /fhir/r4/{tenant}/TestScript/$evaluate
// and GET/POST /fhir/r4/{tenant}/TestScript/{id}/$evaluate, executing an HL7 FHIR R4
// TestScript (setup, test[], teardown, operations, variables, and FHIRPath/HTTP assertions)
// and returning an HL7 FHIR R4 TestReport resource.
func (s *Server) handleTestScriptEvaluate(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	scriptID := strings.TrimSpace(r.PathValue("id"))
	if qID := strings.TrimSpace(r.URL.Query().Get("id")); qID != "" && scriptID == "" {
		scriptID = cleanID(qID)
	}

	if secCtx, hasSecCtx := auth.FromContext(r.Context()); hasSecCtx {
		if !auth.Allows(secCtx, "TestScript", auth.ActionRead) && !auth.Allows(secCtx, "TestScript", auth.ActionSearch) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope to execute TestScript/$evaluate")
			return
		}
	}

	var scriptMap map[string]any
	externalFixtures := make(map[string]map[string]any)

	if r.Method == http.MethodPost && r.Body != nil && r.ContentLength != 0 {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read request body")
			return
		}
		if len(bytes.TrimSpace(body)) > 0 {
			var raw map[string]any
			if err := json.Unmarshal(body, &raw); err != nil {
				writeOperationOutcome(w, http.StatusBadRequest, "invalid", "invalid JSON body for TestScript/$evaluate")
				return
			}
			switch rt, _ := raw["resourceType"].(string); rt {
			case "TestScript":
				scriptMap = raw
			case "Parameters":
				if params, ok := raw["parameter"].([]any); ok {
					for _, p := range params {
						pm, ok := p.(map[string]any)
						if !ok {
							continue
						}
						name, _ := pm["name"].(string)
						switch name {
						case "testScript", "resource":
							if rm, ok := pm["resource"].(map[string]any); ok {
								scriptMap = rm
							}
						case "id":
							if scriptID == "" {
								scriptID = cleanID(extractParamString(pm))
							}
						case "fixture":
							if parts, ok := pm["part"].([]any); ok {
								var fID string
								var fRes map[string]any
								for _, pt := range parts {
									ptm, ok := pt.(map[string]any)
									if !ok {
										continue
									}
									pName, _ := ptm["name"].(string)
									switch pName {
									case "id":
										fID = extractParamString(ptm)
									case "resource":
										fRes, _ = ptm["resource"].(map[string]any)
									}
								}
								if fID != "" && fRes != nil {
									externalFixtures[fID] = fRes
								}
							}
						}
					}
				}
			default:
				writeOperationOutcome(w, http.StatusBadRequest, "invalid",
					"TestScript/$evaluate body must be a TestScript or Parameters resource")
				return
			}
		}
	}

	if scriptMap == nil {
		if scriptID == "" {
			writeOperationOutcome(w, http.StatusBadRequest, "required",
				"TestScript/$evaluate requires an inline TestScript payload or a TestScript/{id} path")
			return
		}
		if s.store == nil {
			writeOperationOutcome(w, http.StatusServiceUnavailable, "exception", "store not configured")
			return
		}
		rec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, "TestScript", scriptID)
		if errors.Is(err, sql.ErrNoRows) {
			writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("TestScript/%s not found", scriptID))
			return
		}
		if errors.Is(err, postgres.ErrResourceDeleted) {
			writeOperationOutcome(w, http.StatusGone, "deleted", fmt.Sprintf("TestScript/%s has been deleted", scriptID))
			return
		}
		if err != nil {
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to load TestScript")
			return
		}
		jb, err := s.protoToJSONWithMeta("TestScript", rec.ResourceProto, rec.ResVersion, rec.LastUpdated)
		if err != nil || json.Unmarshal(jb, &scriptMap) != nil {
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to decode TestScript")
			return
		}
	}

	report := s.executeTestScript(r, tenant, scriptMap, externalFixtures)

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(report)
}

func (s *Server) executeTestScript(
	origReq *http.Request,
	tenant string,
	script map[string]any,
	externalFixtures map[string]map[string]any,
) map[string]any {
	now := time.Now().UTC().Format(time.RFC3339)
	scriptID, _ := script["id"].(string)
	if scriptID == "" {
		scriptID = "inline-testscript"
	}
	scriptName, _ := script["name"].(string)
	if scriptName == "" {
		scriptName = scriptID
	}

	// Build fixture table from contained resources, static fixtures, and external fixtures
	containedByID := make(map[string]map[string]any)
	if contained, ok := script["contained"].([]any); ok {
		for _, c := range contained {
			if cm, ok := c.(map[string]any); ok {
				if cid, _ := cm["id"].(string); cid != "" {
					containedByID[cid] = cm
				}
			}
		}
	}

	fixtures := make(map[string]*testScriptInteractionCapture)
	for fID, fRes := range externalFixtures {
		rt, _ := fRes["resourceType"].(string)
		rid, _ := fRes["id"].(string)
		raw, _ := json.Marshal(fRes)
		fixtures[fID] = &testScriptInteractionCapture{
			statusCode: http.StatusOK,
			rawBody:    raw,
			bodyMap:    fRes,
			resType:    rt,
			resID:      rid,
		}
	}
	if fList, ok := script["fixture"].([]any); ok {
		for _, f := range fList {
			fm, ok := f.(map[string]any)
			if !ok {
				continue
			}
			fID, _ := fm["id"].(string)
			if fID == "" {
				continue
			}
			if refMap, ok := fm["resource"].(map[string]any); ok {
				// Inline resource or reference to #contained
				if rt, _ := refMap["resourceType"].(string); rt != "" {
					rid, _ := refMap["id"].(string)
					raw, _ := json.Marshal(refMap)
					fixtures[fID] = &testScriptInteractionCapture{
						statusCode: http.StatusOK,
						rawBody:    raw,
						bodyMap:    refMap,
						resType:    rt,
						resID:      rid,
					}
				} else if refStr, _ := refMap["reference"].(string); strings.HasPrefix(refStr, "#") {
					if cm, ok := containedByID[strings.TrimPrefix(refStr, "#")]; ok {
						rt, _ := cm["resourceType"].(string)
						rid, _ := cm["id"].(string)
						raw, _ := json.Marshal(cm)
						fixtures[fID] = &testScriptInteractionCapture{
							statusCode: http.StatusOK,
							rawBody:    raw,
							bodyMap:    cm,
							resType:    rt,
							resID:      rid,
						}
					}
				}
			}
		}
	}

	// Build variable definitions
	type tsVarDef struct {
		name         string
		defaultValue string
		expression   string
		headerField  string
		sourceID     string
	}
	var varDefs []tsVarDef
	vars := make(map[string]string)
	if vList, ok := script["variable"].([]any); ok {
		for _, v := range vList {
			vm, ok := v.(map[string]any)
			if !ok {
				continue
			}
			vd := tsVarDef{
				name:         extractStringField(vm, "name"),
				defaultValue: extractStringField(vm, "defaultValue"),
				expression:   extractStringField(vm, "expression"),
				headerField:  extractStringField(vm, "headerField"),
				sourceID:     extractStringField(vm, "sourceId"),
			}
			if vd.name != "" {
				varDefs = append(varDefs, vd)
				if vd.defaultValue != "" {
					vars[vd.name] = vd.defaultValue
				}
			}
		}
	}

	refreshVariables := func() {
		for _, vd := range varDefs {
			if vd.sourceID == "" {
				continue
			}
			capFix, ok := fixtures[vd.sourceID]
			if !ok || capFix == nil {
				continue
			}
			if vd.headerField != "" && capFix.headers != nil {
				if hv := capFix.headers.Get(vd.headerField); hv != "" {
					vars[vd.name] = hv
				}
			}
			if vd.expression != "" && capFix.bodyMap != nil {
				vals := evalFHIRPath(capFix.bodyMap, vd.expression)
				if len(vals) > 0 {
					if sVal, ok := toStringConv(vals[0]); ok && sVal != "" {
						vars[vd.name] = sVal
					}
				}
			}
		}
	}

	expandVars := func(in string) string {
		refreshVariables()
		out := in
		for k, v := range vars {
			out = strings.ReplaceAll(out, "${"+k+"}", v)
		}
		return out
	}

	var lastCapture *testScriptInteractionCapture
	totalChecks := 0
	passedChecks := 0
	overallPass := true

	runActions := func(actions []any, isTeardown bool) []map[string]any {
		var actionReports []map[string]any
		for _, act := range actions {
			am, ok := act.(map[string]any)
			if !ok {
				continue
			}
			ar := make(map[string]any)
			if opMap, hasOp := am["operation"].(map[string]any); hasOp {
				capRes, opReport := s.executeTestScriptOperation(origReq, tenant, opMap, fixtures, containedByID, expandVars)
				lastCapture = capRes
				ar["operation"] = opReport
				if !isTeardown {
					totalChecks++
					if opReport["result"] == "pass" {
						passedChecks++
					} else {
						overallPass = false
					}
				}
			}
			if assertMap, hasAssert := am["assert"].(map[string]any); hasAssert && !isTeardown {
				assertReport := s.executeTestScriptAssert(assertMap, lastCapture, fixtures, expandVars)
				ar["assert"] = assertReport
				totalChecks++
				resStr, _ := assertReport["result"].(string)
				if resStr == "pass" || resStr == "warning" {
					passedChecks++
				} else {
					overallPass = false
				}
			}
			if len(ar) > 0 {
				actionReports = append(actionReports, ar)
			}
		}
		return actionReports
	}

	reportID := "tr-" + uuid.NewString()[:8]
	overallResult := "pass"

	var setupReport map[string]any
	if setupMap, ok := script["setup"].(map[string]any); ok {
		if actions, ok := setupMap["action"].([]any); ok {
			actReps := runActions(actions, false)
			if len(actReps) > 0 {
				setupReport = map[string]any{"action": actReps}
			}
		}
	}

	var testReports []map[string]any
	if testList, ok := script["test"].([]any); ok {
		for idx, tItem := range testList {
			tm, ok := tItem.(map[string]any)
			if !ok {
				continue
			}
			tName := extractStringField(tm, "name")
			if tName == "" {
				tName = fmt.Sprintf("test-%d", idx+1)
			}
			tDesc := extractStringField(tm, "description")
			actions, _ := tm["action"].([]any)
			actReps := runActions(actions, false)
			trEntry := map[string]any{
				"name":   tName,
				"action": actReps,
			}
			if tDesc != "" {
				trEntry["description"] = tDesc
			}
			testReports = append(testReports, trEntry)
		}
	}

	var teardownReport map[string]any
	if tdMap, ok := script["teardown"].(map[string]any); ok {
		if actions, ok := tdMap["action"].([]any); ok {
			actReps := runActions(actions, true)
			if len(actReps) > 0 {
				teardownReport = map[string]any{"action": actReps}
			}
		}
	}

	if !overallPass {
		overallResult = "fail"
	}
	score := 100.0
	if totalChecks > 0 {
		score = float64(passedChecks) * 100.0 / float64(totalChecks)
	}

	report := map[string]any{
		"resourceType": "TestReport",
		"id":           reportID,
		"status":       "completed",
		"testScript": map[string]any{
			"reference": "TestScript/" + scriptID,
			"display":   scriptName,
		},
		"result": overallResult,
		"score":  score,
		"tester": "Flint FHIR R4 TestScript Conformance Engine",
		"issued": now,
	}
	if setupReport != nil {
		report["setup"] = setupReport
	}
	if len(testReports) > 0 {
		report["test"] = testReports
	}
	if teardownReport != nil {
		report["teardown"] = teardownReport
	}
	return report
}

func (s *Server) executeTestScriptOperation(
	origReq *http.Request,
	tenant string,
	opMap map[string]any,
	fixtures map[string]*testScriptInteractionCapture,
	containedByID map[string]map[string]any,
	expandVars func(string) string,
) (*testScriptInteractionCapture, map[string]any) {
	opCode := extractCodingOrString(opMap["type"])
	if opCode == "" {
		opCode = "read"
	}
	resType := canonicalR4ResourceType(expandVars(extractStringField(opMap, "resource")))
	targetID := expandVars(extractStringField(opMap, "targetId"))
	sourceID := expandVars(extractStringField(opMap, "sourceId"))
	responseID := expandVars(extractStringField(opMap, "responseId"))
	params := expandVars(extractStringField(opMap, "params"))
	explicitURL := expandVars(extractStringField(opMap, "url"))

	// Resolve targetId from captured fixture if targetId matches a fixture key
	resolvedID := targetID
	if fix, ok := fixtures[targetID]; ok && fix != nil {
		if fix.resID != "" {
			resolvedID = fix.resID
		}
		if resType == "" && fix.resType != "" {
			resType = fix.resType
		}
	}

	// Resolve request body from sourceId fixture or contained resource
	var bodyMap map[string]any
	if sourceID != "" {
		if fix, ok := fixtures[sourceID]; ok && fix != nil && fix.bodyMap != nil {
			clonedBytes, _ := json.Marshal(fix.bodyMap)
			_ = json.Unmarshal(clonedBytes, &bodyMap)
		} else if cm, ok := containedByID[strings.TrimPrefix(sourceID, "#")]; ok {
			clonedBytes, _ := json.Marshal(cm)
			_ = json.Unmarshal(clonedBytes, &bodyMap)
		}
	}
	if resType == "" && bodyMap != nil {
		resType, _ = bodyMap["resourceType"].(string)
	}

	basePath := "/fhir/r4/" + tenant
	method := http.MethodGet
	var targetPath string

	if explicitURL != "" {
		if strings.HasPrefix(explicitURL, "/fhir/r4/") {
			targetPath = explicitURL
		} else {
			targetPath = basePath + "/" + strings.TrimPrefix(explicitURL, "/")
		}
	} else {
		switch strings.ToLower(opCode) {
		case "create":
			method = http.MethodPost
			targetPath = basePath + "/" + resType
			if params != "" {
				if !strings.HasPrefix(params, "?") {
					params = "?" + params
				}
				targetPath += params
			}
		case "read":
			method = http.MethodGet
			targetPath = basePath + "/" + resType + "/" + resolvedID
		case "vread":
			method = http.MethodGet
			vid := strings.TrimPrefix(params, "/")
			if !strings.HasPrefix(vid, "_history/") {
				vid = "_history/" + strings.TrimPrefix(vid, "?")
			}
			targetPath = basePath + "/" + resType + "/" + resolvedID + "/" + vid
		case "update", "updatecreate":
			method = http.MethodPut
			if resolvedID != "" {
				if bodyMap != nil {
					bodyMap["id"] = resolvedID
				}
				targetPath = basePath + "/" + resType + "/" + resolvedID
			} else {
				if !strings.HasPrefix(params, "?") && params != "" {
					params = "?" + params
				}
				targetPath = basePath + "/" + resType + params
			}
		case "patch":
			method = http.MethodPatch
			targetPath = basePath + "/" + resType + "/" + resolvedID
		case "delete":
			method = http.MethodDelete
			if resolvedID != "" {
				targetPath = basePath + "/" + resType + "/" + resolvedID
			} else {
				if !strings.HasPrefix(params, "?") && params != "" {
					params = "?" + params
				}
				targetPath = basePath + "/" + resType + params
			}
		case "search":
			method = http.MethodGet
			if params != "" && !strings.HasPrefix(params, "?") {
				params = "?" + params
			}
			if resType != "" {
				targetPath = basePath + "/" + resType + params
			} else {
				targetPath = basePath + params
			}
		case "history":
			method = http.MethodGet
			if resolvedID != "" {
				targetPath = basePath + "/" + resType + "/" + resolvedID + "/_history"
			} else if resType != "" {
				targetPath = basePath + "/" + resType + "/_history"
			} else {
				targetPath = basePath + "/_history"
			}
		case "transaction", "batch":
			method = http.MethodPost
			targetPath = basePath
		case "capabilities", "metadata":
			method = http.MethodGet
			targetPath = basePath + "/metadata"
		default:
			// Extended operation such as $validate, $fhirpath, $everything
			opSlug := opCode
			if !strings.HasPrefix(opSlug, "$") {
				opSlug = "$" + opSlug
			}
			if bodyMap != nil {
				method = http.MethodPost
			} else {
				method = http.MethodGet
			}
			if resolvedID != "" && resType != "" {
				targetPath = basePath + "/" + resType + "/" + resolvedID + "/" + opSlug
			} else if resType != "" {
				targetPath = basePath + "/" + resType + "/" + opSlug
			} else {
				targetPath = basePath + "/" + opSlug
			}
			if params != "" {
				if !strings.HasPrefix(params, "?") {
					params = "?" + params
				}
				targetPath += params
			}
		}
	}

	if explicitMethod := strings.ToUpper(extractStringField(opMap, "method")); explicitMethod != "" {
		method = explicitMethod
	}

	var reqReader io.Reader
	if bodyMap != nil && (method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch) {
		rawReq, _ := json.Marshal(bodyMap)
		reqReader = bytes.NewReader(rawReq)
	}

	req := httptest.NewRequest(method, targetPath, reqReader)
	if origReq != nil {
		req = req.WithContext(origReq.Context())
		if authHdr := origReq.Header.Get("Authorization"); authHdr != "" {
			req.Header.Set("Authorization", authHdr)
		}
	}
	contentType := extractStringField(opMap, "contentType")
	if contentType == "" && bodyMap != nil {
		contentType = "application/fhir+json"
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if accept := extractStringField(opMap, "accept"); accept != "" {
		req.Header.Set("Accept", accept)
	}
	if hdrs, ok := opMap["requestHeader"].([]any); ok {
		for _, h := range hdrs {
			if hm, ok := h.(map[string]any); ok {
				f := extractStringField(hm, "field")
				v := expandVars(extractStringField(hm, "value"))
				if f != "" {
					req.Header.Set(f, v)
				}
			}
		}
	}

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	rawResp := rec.Body.Bytes()
	var respMap map[string]any
	_ = json.Unmarshal(rawResp, &respMap)

	outRT, _ := respMap["resourceType"].(string)
	outID, _ := respMap["id"].(string)
	if outID == "" && resolvedID != "" {
		outID = resolvedID
	}

	capRes := &testScriptInteractionCapture{
		reqMethod:  method,
		reqURL:     targetPath,
		reqHeaders: req.Header.Clone(),
		reqBody:    bodyMap,
		statusCode: rec.Code,
		headers:    rec.Header().Clone(),
		rawBody:    rawResp,
		bodyMap:    respMap,
		resType:    outRT,
		resID:      outID,
	}
	if responseID != "" {
		fixtures[responseID] = capRes
	}
	if sourceID != "" && outID != "" {
		if srcFix, ok := fixtures[sourceID]; ok && srcFix != nil && srcFix.resID == "" {
			srcFix.resID = outID
		}
	}

	return capRes, map[string]any{
		"result":  "pass",
		"message": fmt.Sprintf("%s %s -> HTTP %d", method, targetPath, rec.Code),
	}
}

func (s *Server) executeTestScriptAssert(
	assertMap map[string]any,
	lastCapture *testScriptInteractionCapture,
	fixtures map[string]*testScriptInteractionCapture,
	expandVars func(string) string,
) map[string]any {
	warningOnly, _ := assertMap["warningOnly"].(bool)
	failResult := "fail"
	if warningOnly {
		failResult = "warning"
	}

	target := lastCapture
	if srcID := expandVars(extractStringField(assertMap, "sourceId")); srcID != "" {
		if fix, ok := fixtures[srcID]; ok && fix != nil {
			target = fix
		}
	}
	if target == nil {
		return map[string]any{
			"result":  failResult,
			"message": "no HTTP interaction available to assert against",
		}
	}

	direction := strings.ToLower(extractStringField(assertMap, "direction"))
	targetBody := target.bodyMap
	targetHeaders := target.headers
	if direction == "request" {
		targetBody = target.reqBody
		targetHeaders = target.reqHeaders
	}

	operator := strings.ToLower(extractStringField(assertMap, "operator"))
	if operator == "" {
		operator = "equals"
	}
	expectedVal := expandVars(extractStringField(assertMap, "value"))

	// 1. Symbolic HTTP response assertion (e.g. "okay", "created", "noContent", "notFound", "gone")
	if symResp := strings.ToLower(extractStringField(assertMap, "response")); symResp != "" {
		wantCode := symbolicResponseToStatusCode(symResp)
		if wantCode != 0 && target.statusCode != wantCode {
			return map[string]any{
				"result":  failResult,
				"message": fmt.Sprintf("expected response %s (%d), got HTTP %d", symResp, wantCode, target.statusCode),
			}
		}
	}

	// 2. Numeric responseCode assertion (supports "200" or "200,201" or operator="in")
	if respCodeSpec := expandVars(extractStringField(assertMap, "responseCode")); respCodeSpec != "" {
		matchedCode := false
		for part := range strings.SplitSeq(respCodeSpec, ",") {
			if codeInt, err := strconv.Atoi(strings.TrimSpace(part)); err == nil && codeInt == target.statusCode {
				matchedCode = true
				break
			}
		}
		if !matchedCode {
			return map[string]any{
				"result":  failResult,
				"message": fmt.Sprintf("expected responseCode in [%s], got HTTP %d", respCodeSpec, target.statusCode),
			}
		}
	}

	// 3. Resource type assertion
	if wantRes := expandVars(extractStringField(assertMap, "resource")); wantRes != "" {
		gotRes, _ := targetBody["resourceType"].(string)
		if !strings.EqualFold(gotRes, wantRes) {
			return map[string]any{
				"result":  failResult,
				"message": fmt.Sprintf("expected resourceType %q, got %q", wantRes, gotRes),
			}
		}
	}

	// 4. Content-Type assertion
	if wantCT := expandVars(extractStringField(assertMap, "contentType")); wantCT != "" {
		gotCT := ""
		if targetHeaders != nil {
			gotCT = targetHeaders.Get("Content-Type")
		}
		if !strings.Contains(strings.ToLower(gotCT), strings.ToLower(wantCT)) {
			return map[string]any{
				"result":  failResult,
				"message": fmt.Sprintf("expected Content-Type containing %q, got %q", wantCT, gotCT),
			}
		}
	}

	// 5. HTTP Header field assertion
	if hdrField := extractStringField(assertMap, "headerField"); hdrField != "" {
		gotHdr := ""
		if targetHeaders != nil {
			gotHdr = targetHeaders.Get(hdrField)
		}
		if !evaluateAssertOperator([]any{gotHdr}, operator, expectedVal) {
			return map[string]any{
				"result":  failResult,
				"message": fmt.Sprintf("header %s assertion (%s %q) failed: got %q", hdrField, operator, expectedVal, gotHdr),
			}
		}
	}

	// 6. minimumId fixture subset assertion
	if minID := expandVars(extractStringField(assertMap, "minimumId")); minID != "" {
		minFix, ok := fixtures[minID]
		if !ok || minFix == nil || minFix.bodyMap == nil {
			return map[string]any{
				"result":  failResult,
				"message": fmt.Sprintf("minimumId fixture %q not found", minID),
			}
		}
		for k := range minFix.bodyMap {
			if k == "id" || k == "meta" {
				continue
			}
			if _, exists := targetBody[k]; !exists {
				return map[string]any{
					"result":  failResult,
					"message": fmt.Sprintf("response missing required minimumId (%s) element %q", minID, k),
				}
			}
		}
	}

	// 7. FHIRPath expression assertion
	if expr := expandVars(extractStringField(assertMap, "expression")); expr != "" {
		if targetBody == nil {
			return map[string]any{
				"result":  failResult,
				"message": fmt.Sprintf("cannot evaluate expression %q on empty body", expr),
			}
		}
		vals := evalFHIRPath(targetBody, expr)

		// Compare against another fixture if compareToSourceId is provided
		if cmpSrcID := expandVars(extractStringField(assertMap, "compareToSourceId")); cmpSrcID != "" {
			cmpFix, ok := fixtures[cmpSrcID]
			if !ok || cmpFix == nil || cmpFix.bodyMap == nil {
				return map[string]any{
					"result":  failResult,
					"message": fmt.Sprintf("compareToSourceId %q not found", cmpSrcID),
				}
			}
			cmpExpr := expandVars(extractStringField(assertMap, "compareToSourceExpression"))
			if cmpExpr == "" {
				cmpExpr = expr
			}
			cmpVals := evalFHIRPath(cmpFix.bodyMap, cmpExpr)
			if len(cmpVals) > 0 {
				expectedVal, _ = toStringConv(cmpVals[0])
			}
		}

		_, hasExplicitValue := assertMap["value"]
		if !hasExplicitValue && extractStringField(assertMap, "compareToSourceId") == "" &&
			operator != "empty" && operator != "notempty" {
			// Truthiness check on FHIRPath result
			if len(vals) == 0 {
				return map[string]any{
					"result":  failResult,
					"message": fmt.Sprintf("FHIRPath expression %q evaluated to empty collection", expr),
				}
			}
			if b, ok := vals[0].(bool); ok && !b {
				return map[string]any{
					"result":  failResult,
					"message": fmt.Sprintf("FHIRPath expression %q evaluated to false", expr),
				}
			}
		} else if !evaluateAssertOperator(vals, operator, expectedVal) {
			return map[string]any{
				"result":  failResult,
				"message": fmt.Sprintf("FHIRPath expression %q (%s %q) failed with %v", expr, operator, expectedVal, vals),
			}
		}
	}

	return map[string]any{
		"result":  "pass",
		"message": "assertion passed",
	}
}

func evaluateAssertOperator(vals []any, operator, expected string) bool {
	switch strings.ToLower(operator) {
	case "empty":
		if len(vals) == 0 {
			return true
		}
		s, _ := toStringConv(vals[0])
		return s == ""
	case "notempty":
		if len(vals) == 0 {
			return false
		}
		s, _ := toStringConv(vals[0])
		return s != ""
	}
	if len(vals) == 0 {
		return false
	}
	actualStr, _ := toStringConv(vals[0])
	switch strings.ToLower(operator) {
	case "equals", "=", "==":
		if actualStr == expected {
			return true
		}
		if af, ok := toFloat(vals[0]); ok {
			if ef, err := strconv.ParseFloat(expected, 64); err == nil && af == ef {
				return true
			}
		}
		return false
	case "notequals", "!=":
		return actualStr != expected
	case "contains":
		return strings.Contains(actualStr, expected)
	case "notcontains":
		return !strings.Contains(actualStr, expected)
	case "in":
		for p := range strings.SplitSeq(expected, ",") {
			if strings.TrimSpace(p) == actualStr {
				return true
			}
		}
		return false
	case "notin":
		for p := range strings.SplitSeq(expected, ",") {
			if strings.TrimSpace(p) == actualStr {
				return false
			}
		}
		return true
	case "greaterthan", ">":
		af, ok := toFloat(vals[0])
		ef, err := strconv.ParseFloat(expected, 64)
		if ok && err == nil {
			return af > ef
		}
		return actualStr > expected
	case "lessthan", "<":
		af, ok := toFloat(vals[0])
		ef, err := strconv.ParseFloat(expected, 64)
		if ok && err == nil {
			return af < ef
		}
		return actualStr < expected
	}
	return actualStr == expected
}

func symbolicResponseToStatusCode(sym string) int {
	switch strings.ToLower(strings.TrimSpace(sym)) {
	case "okay", "ok":
		return http.StatusOK
	case "created":
		return http.StatusCreated
	case "nocontent", "no-content":
		return http.StatusNoContent
	case "notmodified", "not-modified":
		return http.StatusNotModified
	case "bad", "badrequest":
		return http.StatusBadRequest
	case "unauthorized":
		return http.StatusUnauthorized
	case "forbidden":
		return http.StatusForbidden
	case "notfound", "not-found":
		return http.StatusNotFound
	case "methodnotallowed":
		return http.StatusMethodNotAllowed
	case "conflict":
		return http.StatusConflict
	case "gone":
		return http.StatusGone
	case "preconditionfailed":
		return http.StatusPreconditionFailed
	case "unprocessable":
		return http.StatusUnprocessableEntity
	}
	return 0
}

func extractStringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func extractCodingOrString(v any) string {
	switch val := v.(type) {
	case string:
		return strings.TrimSpace(val)
	case map[string]any:
		if c, ok := val["code"].(string); ok && c != "" {
			return strings.TrimSpace(c)
		}
		if codings, ok := val["coding"].([]any); ok && len(codings) > 0 {
			if cm, ok := codings[0].(map[string]any); ok {
				if c, ok := cm["code"].(string); ok {
					return strings.TrimSpace(c)
				}
			}
		}
	}
	return ""
}
