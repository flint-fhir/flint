package server

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

type measurePopulationCriteriaSpec struct {
	id          string
	code        string
	display     string
	codeConcept map[string]any
	language    string
	expression  string
}

type measureStratifierComponentSpec struct {
	id         string
	code       map[string]any
	expression string
}

type measureStratifierSpec struct {
	id         string
	code       map[string]any
	expression string
	components []measureStratifierComponentSpec
}

type measureGroupSpec struct {
	id                string
	code              map[string]any
	populations       []measurePopulationCriteriaSpec
	hasCustomFHIRPath bool
	hasDenomException bool
	hasNumExclusion   bool
	stratifiers       []measureStratifierSpec
}

type measureSupplementalDataSpec struct {
	id         string
	code       map[string]any
	usage      []map[string]any
	expression string
}

type evaluatedSubjectOutcome struct {
	patientID string
	inInitPop bool
	inDenom   bool
	inExcl    bool
	inExcep   bool
	inNum     bool
	inNumExcl bool
	evalRefs  []string
	evalRoot  map[string]any
	snap      patientClinicalSnapshot
}

type canonicalConcept struct {
	key     string
	concept map[string]any
}

type popAccumulator struct {
	initPop   int
	denom     int
	denomExcl int
	denomExcp int
	num       int
	numExcl   int

	initPopIDs   []string
	denomIDs     []string
	denomExclIDs []string
	denomExcpIDs []string
	numIDs       []string
	numExclIDs   []string
}

func (a *popAccumulator) add(o evaluatedSubjectOutcome) {
	if o.inInitPop {
		a.initPop++
		a.initPopIDs = append(a.initPopIDs, o.patientID)
	}
	if o.inDenom {
		a.denom++
		a.denomIDs = append(a.denomIDs, o.patientID)
	}
	if o.inExcl {
		a.denomExcl++
		a.denomExclIDs = append(a.denomExclIDs, o.patientID)
	}
	if o.inExcep {
		a.denomExcp++
		a.denomExcpIDs = append(a.denomExcpIDs, o.patientID)
	}
	if o.inNum {
		a.num++
		a.numIDs = append(a.numIDs, o.patientID)
	}
	if o.inNumExcl {
		a.numExcl++
		a.numExclIDs = append(a.numExclIDs, o.patientID)
	}
}

func (a *popAccumulator) proportionScore() float64 {
	effectiveDenom := a.denom - a.denomExcl - a.denomExcp
	if effectiveDenom <= 0 {
		return 0.0
	}
	return math.Round((float64(a.num)/float64(effectiveDenom))*10000) / 10000
}

func defaultBuiltInStratifiers() []measureStratifierSpec {
	return []measureStratifierSpec{
		{
			id: "stratifier-gender",
			code: map[string]any{
				"coding": []any{
					map[string]any{
						"system":  "http://hl7.org/fhir/administrative-gender",
						"code":    "gender",
						"display": "Administrative Gender",
					},
				},
				"text": "Administrative Gender",
			},
			expression: "Patient.gender",
		},
	}
}

func defaultBuiltInSupplementalData() []measureSupplementalDataSpec {
	return []measureSupplementalDataSpec{
		{
			id: "sde-sex",
			code: map[string]any{
				"coding": []any{
					map[string]any{
						"system":  "http://loinc.org",
						"code":    "76689-9",
						"display": "Sex assigned at birth",
					},
				},
				"text": "sde-sex",
			},
			expression: "Patient.gender",
		},
		{
			id: "sde-race",
			code: map[string]any{
				"coding": []any{
					map[string]any{
						"system":  "urn:oid:2.16.840.1.113883.6.238",
						"code":    "race",
						"display": "Race",
					},
				},
				"text": "sde-race",
			},
			expression: "Patient.extension.where(url = 'http://hl7.org/fhir/us/core/StructureDefinition/us-core-race').extension.where(url = 'ombCategory').valueCoding",
		},
		{
			id: "sde-ethnicity",
			code: map[string]any{
				"coding": []any{
					map[string]any{
						"system":  "urn:oid:2.16.840.1.113883.6.238",
						"code":    "ethnicity",
						"display": "Ethnicity",
					},
				},
				"text": "sde-ethnicity",
			},
			expression: "Patient.extension.where(url = 'http://hl7.org/fhir/us/core/StructureDefinition/us-core-ethnicity').extension.where(url = 'ombCategory').valueCoding",
		},
		{
			id: "sde-payer",
			code: map[string]any{
				"coding": []any{
					map[string]any{
						"system":  "http://terminology.hl7.org/CodeSystem/coverage-selfpay",
						"code":    "payer",
						"display": "Payer",
					},
				},
				"text": "sde-payer",
			},
			expression: "Coverage.type",
		},
	}
}

func parseMeasureDetailsFromMap(m map[string]any, spec *measureSpec) {
	if m == nil || spec == nil {
		return
	}
	if impNot, ok := m["improvementNotation"].(map[string]any); ok {
		for _, c := range extractCodingsFromCodeableConcept(impNot) {
			switch strings.ToLower(strings.TrimSpace(c.code)) {
			case "increase", "decrease":
				spec.improvementNotation = strings.ToLower(strings.TrimSpace(c.code))
			}
		}
	}

	if rawGroups, ok := m["group"].([]any); ok {
		for gIdx, rawG := range rawGroups {
			gm, ok := rawG.(map[string]any)
			if !ok {
				continue
			}
			gid, _ := gm["id"].(string)
			gid = strings.TrimSpace(gid)
			if gid == "" {
				gid = fmt.Sprintf("group-%d", gIdx+1)
			}
			gs := measureGroupSpec{id: gid}
			if gCode, ok := gm["code"].(map[string]any); ok {
				gs.code = gCode
			}
			if rawPops, ok := gm["population"].([]any); ok {
				for pIdx, rawP := range rawPops {
					pm, ok := rawP.(map[string]any)
					if !ok {
						continue
					}
					pid, _ := pm["id"].(string)
					pCodeMap, _ := pm["code"].(map[string]any)
					popCode, popDisp := extractMeasurePopulationCode(pCodeMap)
					if popCode == "" {
						continue
					}
					if pid == "" {
						pid = fmt.Sprintf("%s-%d", popCode, pIdx+1)
					}
					var expr, lang string
					if crit, ok := pm["criteria"].(map[string]any); ok {
						expr, _ = crit["expression"].(string)
						lang, _ = crit["language"].(string)
						expr = strings.TrimSpace(expr)
						lang = strings.ToLower(strings.TrimSpace(lang))
					}
					if expr != "" && (lang == "" || strings.Contains(lang, "fhirpath")) {
						gs.hasCustomFHIRPath = true
					}
					if popCode == "denominator-exception" {
						gs.hasDenomException = true
					}
					if popCode == "numerator-exclusion" {
						gs.hasNumExclusion = true
					}
					gs.populations = append(gs.populations, measurePopulationCriteriaSpec{
						id:          pid,
						code:        popCode,
						display:     popDisp,
						codeConcept: pCodeMap,
						language:    lang,
						expression:  expr,
					})
				}
			}
			if rawStrats, ok := gm["stratifier"].([]any); ok {
				for sIdx, rawS := range rawStrats {
					sm, ok := rawS.(map[string]any)
					if !ok {
						continue
					}
					sid, _ := sm["id"].(string)
					sid = strings.TrimSpace(sid)
					if sid == "" {
						sid = fmt.Sprintf("stratifier-%d", sIdx+1)
					}
					sCode, _ := sm["code"].(map[string]any)
					var sExpr string
					if crit, ok := sm["criteria"].(map[string]any); ok {
						sExpr, _ = crit["expression"].(string)
						sExpr = strings.TrimSpace(sExpr)
					}
					var comps []measureStratifierComponentSpec
					if rawComps, ok := sm["component"].([]any); ok {
						for cIdx, rawC := range rawComps {
							cm, ok := rawC.(map[string]any)
							if !ok {
								continue
							}
							cid, _ := cm["id"].(string)
							cid = strings.TrimSpace(cid)
							if cid == "" {
								cid = fmt.Sprintf("comp-%d", cIdx+1)
							}
							cCode, _ := cm["code"].(map[string]any)
							if cCode == nil {
								cCode = map[string]any{"text": cid}
							}
							var cExpr string
							if crit, ok := cm["criteria"].(map[string]any); ok {
								cExpr, _ = crit["expression"].(string)
								cExpr = strings.TrimSpace(cExpr)
							}
							if cExpr != "" {
								comps = append(comps, measureStratifierComponentSpec{
									id:         cid,
									code:       cCode,
									expression: cExpr,
								})
							}
						}
					}
					if sExpr != "" || len(comps) > 0 {
						gs.stratifiers = append(gs.stratifiers, measureStratifierSpec{
							id:         sid,
							code:       sCode,
							expression: sExpr,
							components: comps,
						})
					}
				}
			}
			spec.groups = append(spec.groups, gs)
		}
	}

	if rawSDEs, ok := m["supplementalData"].([]any); ok {
		for idx, rawSD := range rawSDEs {
			sdm, ok := rawSD.(map[string]any)
			if !ok {
				continue
			}
			sdID, _ := sdm["id"].(string)
			sdID = strings.TrimSpace(sdID)
			sdCode, _ := sdm["code"].(map[string]any)
			if sdID == "" && sdCode != nil {
				if txt, _ := sdCode["text"].(string); strings.TrimSpace(txt) != "" {
					sdID = sanitizeFHIRID(txt)
				} else if codings := extractCodingsFromCodeableConcept(sdCode); len(codings) > 0 && codings[0].code != "" {
					sdID = sanitizeFHIRID(codings[0].code)
				}
			}
			if sdID == "" {
				sdID = fmt.Sprintf("sde-%d", idx+1)
			}
			if sdCode == nil {
				sdCode = map[string]any{"text": sdID}
			}
			var sdExpr string
			if crit, ok := sdm["criteria"].(map[string]any); ok {
				sdExpr, _ = crit["expression"].(string)
				sdExpr = strings.TrimSpace(sdExpr)
			}
			if sdExpr == "" {
				continue
			}
			var usages []map[string]any
			if rawU, ok := sdm["usage"].([]any); ok {
				for _, u := range rawU {
					if um, ok := u.(map[string]any); ok {
						usages = append(usages, um)
					}
				}
			}
			spec.supplementalData = append(spec.supplementalData, measureSupplementalDataSpec{
				id:         sdID,
				code:       sdCode,
				usage:      usages,
				expression: sdExpr,
			})
		}
	}
}

func extractCodingsFromCodeableConcept(cc map[string]any) []simpleCoding {
	if cc == nil {
		return nil
	}
	return extractCodingsFromResource(map[string]any{"code": cc})
}

func extractMeasurePopulationCode(cc map[string]any) (code, display string) {
	for _, c := range extractCodingsFromCodeableConcept(cc) {
		norm := strings.ToLower(strings.TrimSpace(c.code))
		switch norm {
		case "initial-population":
			return norm, nonEmptyOr(c.display, "Initial Population")
		case "denominator":
			return norm, nonEmptyOr(c.display, "Denominator")
		case "denominator-exclusion":
			return norm, nonEmptyOr(c.display, "Denominator Exclusion")
		case "denominator-exception":
			return norm, nonEmptyOr(c.display, "Denominator Exception")
		case "numerator":
			return norm, nonEmptyOr(c.display, "Numerator")
		case "numerator-exclusion":
			return norm, nonEmptyOr(c.display, "Numerator Exclusion")
		}
	}
	return "", ""
}

func nonEmptyOr(val, fallback string) string {
	if strings.TrimSpace(val) != "" {
		return strings.TrimSpace(val)
	}
	return fallback
}

func sanitizeFHIRID(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-':
			b.WriteRune(r)
		case r == '_' || r == ' ' || r == '.' || r == ':' || r == '/':
			b.WriteByte('-')
		}
	}
	res := strings.Trim(b.String(), "-")
	for strings.Contains(res, "--") {
		res = strings.ReplaceAll(res, "--", "-")
	}
	if res == "" {
		return "item"
	}
	if len(res) > 48 {
		res = strings.TrimRight(res[:48], "-")
	}
	return res
}

func measureNeedsExtraResources(spec *measureSpec) (needProc, needMedReq bool) {
	if spec == nil {
		return false, false
	}
	checkExpr := func(expr string) {
		if strings.Contains(expr, "Procedure") {
			needProc = true
		}
		if strings.Contains(expr, "MedicationRequest") {
			needMedReq = true
		}
	}
	for _, g := range spec.groups {
		for _, p := range g.populations {
			checkExpr(p.expression)
		}
		for _, st := range g.stratifiers {
			checkExpr(st.expression)
			for _, comp := range st.components {
				checkExpr(comp.expression)
			}
		}
	}
	for _, sd := range spec.supplementalData {
		checkExpr(sd.expression)
	}
	return needProc, needMedReq
}

func toAnySlice(maps []map[string]any) []any {
	if len(maps) == 0 {
		return []any{}
	}
	out := make([]any, len(maps))
	for i, m := range maps {
		out[i] = m
	}
	return out
}

func filterResourcesInPeriod(resources []map[string]any, periodStart, periodEnd time.Time) []map[string]any {
	if len(resources) == 0 {
		return nil
	}
	var out []map[string]any
	for _, res := range resources {
		if resourceInPeriod(res, periodStart, periodEnd) {
			out = append(out, res)
		}
	}
	return out
}

func buildMeasurePatientEvalRoot(snap patientClinicalSnapshot, periodStart, periodEnd time.Time) map[string]any {
	root := make(map[string]any, len(snap.patient)+8)
	for k, v := range snap.patient {
		root[k] = v
	}
	root["resourceType"] = "Patient"
	if root["id"] == nil && snap.patientID != "" {
		root["id"] = snap.patientID
	}
	root["Condition"] = toAnySlice(snap.conditions)
	root["Encounter"] = toAnySlice(filterResourcesInPeriod(snap.encounters, periodStart, periodEnd))
	root["Observation"] = toAnySlice(filterResourcesInPeriod(snap.observations, periodStart, periodEnd))
	root["Coverage"] = toAnySlice(snap.coverages)
	root["Procedure"] = toAnySlice(filterResourcesInPeriod(snap.procedures, periodStart, periodEnd))
	root["MedicationRequest"] = toAnySlice(filterResourcesInPeriod(snap.medRequests, periodStart, periodEnd))
	return root
}

func collectSnapshotRefs(snap patientClinicalSnapshot, periodStart, periodEnd time.Time) []string {
	var refs []string
	seen := make(map[string]bool)
	add := func(resType string, list []map[string]any, checkPeriod bool) {
		for _, m := range list {
			if checkPeriod && !resourceInPeriod(m, periodStart, periodEnd) {
				continue
			}
			if id, _ := m["id"].(string); id != "" {
				ref := resType + "/" + id
				if !seen[ref] {
					seen[ref] = true
					refs = append(refs, ref)
				}
			}
		}
	}
	add("Encounter", snap.encounters, true)
	add("Condition", snap.conditions, false)
	add("Observation", snap.observations, true)
	add("Coverage", snap.coverages, false)
	add("Procedure", snap.procedures, true)
	add("MedicationRequest", snap.medRequests, true)
	return refs
}

func evalMeasurePredicate(evalRoot map[string]any, expr string) bool {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return false
	}
	ok, err := EvalFHIRPathPredicate(evalRoot, nil, expr)
	if err == nil {
		return ok
	}
	// If expression returned multiple matching items (e.g., Observation.where(...)), non-empty collection means true in CQM population membership!
	vals, tErr := EvalFHIRPathTyped(evalRoot, expr)
	if tErr != nil || len(vals) == 0 {
		return false
	}
	for _, v := range vals {
		if b, isBool := v.Value.(bool); isBool {
			if b {
				return true
			}
			continue
		}
		if v.Value != nil {
			return true
		}
	}
	return false
}

func evaluateSubjectAgainstGroup(
	spec *measureSpec,
	group measureGroupSpec,
	snap patientClinicalSnapshot,
	periodStart, periodEnd time.Time,
) evaluatedSubjectOutcome {
	evalRoot := buildMeasurePatientEvalRoot(snap, periodStart, periodEnd)
	out := evaluatedSubjectOutcome{
		patientID: snap.patientID,
		evalRoot:  evalRoot,
		snap:      snap,
	}

	if !group.hasCustomFHIRPath {
		inInit, inDenom, inExcl, inNum, refs := evaluatePatientForMeasure(spec.kind, snap, periodStart, periodEnd)
		out.inInitPop = inInit
		out.inDenom = inDenom
		out.inExcl = inExcl
		out.inNum = inNum
		out.evalRefs = refs
		return out
	}

	exprByPop := make(map[string]string, len(group.populations))
	for _, p := range group.populations {
		if p.expression != "" {
			exprByPop[p.code] = p.expression
		}
	}

	if initExpr, hasInit := exprByPop["initial-population"]; hasInit {
		out.inInitPop = evalMeasurePredicate(evalRoot, initExpr)
	} else {
		out.inInitPop = true
	}
	if !out.inInitPop {
		return out
	}

	if denomExpr, hasDenom := exprByPop["denominator"]; hasDenom {
		out.inDenom = evalMeasurePredicate(evalRoot, denomExpr)
	} else {
		out.inDenom = out.inInitPop
	}

	if out.inDenom {
		if exclExpr, hasExcl := exprByPop["denominator-exclusion"]; hasExcl {
			out.inExcl = evalMeasurePredicate(evalRoot, exclExpr)
		}
	}

	if out.inDenom && !out.inExcl {
		inNumRaw := false
		if numExpr, hasNum := exprByPop["numerator"]; hasNum {
			inNumRaw = evalMeasurePredicate(evalRoot, numExpr)
		}
		if inNumRaw {
			if numExclExpr, hasNumExcl := exprByPop["numerator-exclusion"]; hasNumExcl {
				out.inNumExcl = evalMeasurePredicate(evalRoot, numExclExpr)
			}
			out.inNum = !out.inNumExcl
		}
		// Denominator exception applies ONLY when subject is in denominator, not excluded, and NOT in numerator.
		if !out.inNum {
			if excepExpr, hasExcep := exprByPop["denominator-exception"]; hasExcep {
				out.inExcep = evalMeasurePredicate(evalRoot, excepExpr)
			}
		}
	}

	out.evalRefs = collectSnapshotRefs(snap, periodStart, periodEnd)
	return out
}

func unknownStratumConcept() canonicalConcept {
	return canonicalConcept{
		key: "http://terminology.hl7.org/CodeSystem/data-absent-reason|unknown",
		concept: map[string]any{
			"coding": []any{
				map[string]any{
					"system":  "http://terminology.hl7.org/CodeSystem/data-absent-reason",
					"code":    "unknown",
					"display": "Unknown",
				},
			},
			"text": "unknown",
		},
	}
}

func genderDisplay(code string) string {
	switch code {
	case "female":
		return "Female"
	case "male":
		return "Male"
	case "other":
		return "Other"
	case "unknown":
		return "Unknown"
	default:
		return code
	}
}

func fhirValueToCanonicalConcept(v any, hintExpr string) (canonicalConcept, bool) {
	v = unwrapFHIRTyped(v)
	if v == nil {
		return canonicalConcept{}, false
	}

	switch tv := v.(type) {
	case map[string]any:
		// Check if it is already a CodeableConcept (has "coding" or "text")
		if codingsRaw, hasCodings := tv["coding"].([]any); hasCodings && len(codingsRaw) > 0 {
			if first, ok := codingsRaw[0].(map[string]any); ok {
				sys, _ := first["system"].(string)
				code, _ := first["code"].(string)
				disp, _ := first["display"].(string)
				txt, _ := tv["text"].(string)
				if txt == "" {
					txt = nonEmptyOr(disp, code)
				}
				cc := map[string]any{
					"coding": codingsRaw,
				}
				if txt != "" {
					cc["text"] = txt
				}
				return canonicalConcept{
					key:     sys + "|" + code + "|" + txt,
					concept: cc,
				}, true
			}
		}
		// Check if it is a FHIR Coding (has "code" string)
		if code, ok := tv["code"].(string); ok && strings.TrimSpace(code) != "" {
			sys, _ := tv["system"].(string)
			disp, _ := tv["display"].(string)
			coding := map[string]any{
				"code": strings.TrimSpace(code),
			}
			if strings.TrimSpace(sys) != "" {
				coding["system"] = strings.TrimSpace(sys)
			}
			if strings.TrimSpace(disp) != "" {
				coding["display"] = strings.TrimSpace(disp)
			}
			txt := nonEmptyOr(disp, code)
			return canonicalConcept{
				key: strings.TrimSpace(sys) + "|" + strings.TrimSpace(code),
				concept: map[string]any{
					"coding": []any{coding},
					"text":   txt,
				},
			}, true
		}
		// Check if CodeableConcept only has "text"
		if txt, ok := tv["text"].(string); ok && strings.TrimSpace(txt) != "" {
			trimmed := strings.TrimSpace(txt)
			return canonicalConcept{
				key: "|" + trimmed,
				concept: map[string]any{
					"coding": []any{map[string]any{"code": trimmed, "display": trimmed}},
					"text":   trimmed,
				},
			}, true
		}
		return canonicalConcept{}, false

	case string:
		s := strings.TrimSpace(tv)
		if s == "" {
			return canonicalConcept{}, false
		}
		lowerExpr := strings.ToLower(hintExpr)
		if strings.Contains(lowerExpr, "gender") {
			low := strings.ToLower(s)
			switch low {
			case "female", "male", "other", "unknown":
				disp := genderDisplay(low)
				return canonicalConcept{
					key: "http://hl7.org/fhir/administrative-gender|" + low,
					concept: map[string]any{
						"coding": []any{
							map[string]any{
								"system":  "http://hl7.org/fhir/administrative-gender",
								"code":    low,
								"display": disp,
							},
						},
						"text": low,
					},
				}, true
			}
		}
		return canonicalConcept{
			key: "|" + s,
			concept: map[string]any{
				"coding": []any{
					map[string]any{
						"code":    s,
						"display": s,
					},
				},
				"text": s,
			},
		}, true

	case bool:
		s := strconv.FormatBool(tv)
		return canonicalConcept{
			key: "bool|" + s,
			concept: map[string]any{
				"coding": []any{map[string]any{"code": s, "display": s}},
				"text":   s,
			},
		}, true

	case int:
		s := strconv.Itoa(tv)
		return canonicalConcept{
			key: "int|" + s,
			concept: map[string]any{
				"coding": []any{map[string]any{"code": s, "display": s}},
				"text":   s,
			},
		}, true

	case int64:
		s := strconv.FormatInt(tv, 10)
		return canonicalConcept{
			key: "int|" + s,
			concept: map[string]any{
				"coding": []any{map[string]any{"code": s, "display": s}},
				"text":   s,
			},
		}, true

	case float64:
		s := strconv.FormatFloat(tv, 'f', -1, 64)
		return canonicalConcept{
			key: "num|" + s,
			concept: map[string]any{
				"coding": []any{map[string]any{"code": s, "display": s}},
				"text":   s,
			},
		}, true
	}

	return canonicalConcept{}, false
}

func evaluateConceptsForExpression(evalRoot map[string]any, snap patientClinicalSnapshot, expr string) []canonicalConcept {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil
	}
	vals, err := EvalFHIRPathTyped(evalRoot, expr)
	var out []canonicalConcept
	seen := make(map[string]bool)
	if err == nil {
		for _, fv := range vals {
			if cc, ok := fhirValueToCanonicalConcept(fv.Value, expr); ok && !seen[cc.key] {
				seen[cc.key] = true
				out = append(out, cc)
			}
		}
	}
	// Fallback for Coverage.type SDE when stored Coverage only specifies payor display/reference
	if len(out) == 0 && expr == "Coverage.type" && len(snap.coverages) > 0 {
		for _, cov := range snap.coverages {
			_, disp := extractPayorAssigner(cov)
			if disp != "" {
				if cc, ok := fhirValueToCanonicalConcept(disp, expr); ok && !seen[cc.key] {
					seen[cc.key] = true
					out = append(out, cc)
				}
			}
		}
	}
	return out
}

func buildPopulationsSliceAndLists(
	acc popAccumulator,
	hasDenomException, hasNumExclusion bool,
	withSubjectList bool,
	listPrefix string,
) (pops []map[string]any, containedLists []map[string]any) {
	type popRow struct {
		code    string
		display string
		count   int
		ids     []string
	}
	rows := []popRow{
		{"initial-population", "Initial Population", acc.initPop, acc.initPopIDs},
		{"denominator", "Denominator", acc.denom, acc.denomIDs},
		{"denominator-exclusion", "Denominator Exclusion", acc.denomExcl, acc.denomExclIDs},
	}
	if hasDenomException {
		rows = append(rows, popRow{"denominator-exception", "Denominator Exception", acc.denomExcp, acc.denomExcpIDs})
	}
	rows = append(rows, popRow{"numerator", "Numerator", acc.num, acc.numIDs})
	if hasNumExclusion {
		rows = append(rows, popRow{"numerator-exclusion", "Numerator Exclusion", acc.numExcl, acc.numExclIDs})
	}

	pops = make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		listID := row.code + "-list"
		if listPrefix != "" {
			listID = listPrefix + "-" + listID
		}
		popObj := map[string]any{
			"code": map[string]any{
				"coding": []map[string]any{
					{
						"system":  "http://terminology.hl7.org/CodeSystem/measure-population",
						"code":    row.code,
						"display": row.display,
					},
				},
			},
			"count": row.count,
		}
		if withSubjectList {
			popObj["subjectResults"] = map[string]any{
				"reference": "#" + listID,
			}
			containedLists = append(containedLists, buildSubjectListResourceWithID(listID, row.display, row.ids))
		}
		pops = append(pops, popObj)
	}
	return pops, containedLists
}

func buildSubjectListResourceWithID(listID, title string, patIDs []string) map[string]any {
	entries := make([]map[string]any, 0, len(patIDs))
	for _, pid := range patIDs {
		entries = append(entries, map[string]any{
			"item": map[string]any{
				"reference": "Patient/" + pid,
			},
		})
	}
	return map[string]any{
		"resourceType": "List",
		"id":           listID,
		"status":       "current",
		"mode":         "snapshot",
		"title":        title,
		"entry":        entries,
	}
}

func evaluateStratifierReport(
	strat measureStratifierSpec,
	outcomes []evaluatedSubjectOutcome,
	hasDenomException, hasNumExclusion bool,
	withSubjectList bool,
	groupPrefix string,
) (stratMap map[string]any, containedLists []map[string]any) {
	stratMap = map[string]any{}
	if strat.id != "" {
		stratMap["id"] = strat.id
	}
	if strat.code != nil {
		stratMap["code"] = []any{strat.code}
	}

	stratIDSlug := sanitizeFHIRID(nonEmptyOr(strat.id, "strat"))
	if groupPrefix != "" {
		stratIDSlug = groupPrefix + "-" + stratIDSlug
	}

	if len(strat.components) == 0 {
		// Single-component stratifier -> MeasureReport.group.stratifier.stratum.value
		type singleBucket struct {
			cc  canonicalConcept
			acc popAccumulator
		}
		buckets := make(map[string]*singleBucket)
		for _, o := range outcomes {
			ccs := evaluateConceptsForExpression(o.evalRoot, o.snap, strat.expression)
			if len(ccs) == 0 {
				ccs = []canonicalConcept{unknownStratumConcept()}
			}
			for _, cc := range ccs {
				b, exists := buckets[cc.key]
				if !exists {
					b = &singleBucket{cc: cc}
					buckets[cc.key] = b
				}
				b.acc.add(o)
			}
		}

		keys := make([]string, 0, len(buckets))
		for k := range buckets {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		strata := make([]map[string]any, 0, len(keys))
		for idx, k := range keys {
			b := buckets[k]
			prefix := fmt.Sprintf("%s-s%d", stratIDSlug, idx+1)
			pops, lists := buildPopulationsSliceAndLists(b.acc, hasDenomException, hasNumExclusion, withSubjectList, prefix)
			containedLists = append(containedLists, lists...)
			strata = append(strata, map[string]any{
				"value":      b.cc.concept,
				"population": pops,
				"measureScore": map[string]any{
					"value": b.acc.proportionScore(),
				},
			})
		}
		stratMap["stratum"] = strata
		return stratMap, containedLists
	}

	// Multi-component stratifier -> MeasureReport.group.stratifier.stratum.component[]
	type compBucket struct {
		compValues []canonicalConcept
		acc        popAccumulator
	}
	buckets := make(map[string]*compBucket)
	for _, o := range outcomes {
		tuple := make([]canonicalConcept, len(strat.components))
		var keyParts []string
		for i, comp := range strat.components {
			ccs := evaluateConceptsForExpression(o.evalRoot, o.snap, comp.expression)
			var chosen canonicalConcept
			if len(ccs) > 0 {
				chosen = ccs[0]
			} else {
				chosen = unknownStratumConcept()
			}
			tuple[i] = chosen
			keyParts = append(keyParts, chosen.key)
		}
		compositeKey := strings.Join(keyParts, "||")
		b, exists := buckets[compositeKey]
		if !exists {
			b = &compBucket{compValues: tuple}
			buckets[compositeKey] = b
		}
		b.acc.add(o)
	}

	keys := make([]string, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	strata := make([]map[string]any, 0, len(keys))
	for idx, k := range keys {
		b := buckets[k]
		compObjs := make([]map[string]any, len(strat.components))
		for i, compSpec := range strat.components {
			compObjs[i] = map[string]any{
				"code":  compSpec.code,
				"value": b.compValues[i].concept,
			}
		}
		prefix := fmt.Sprintf("%s-s%d", stratIDSlug, idx+1)
		pops, lists := buildPopulationsSliceAndLists(b.acc, hasDenomException, hasNumExclusion, withSubjectList, prefix)
		containedLists = append(containedLists, lists...)
		strata = append(strata, map[string]any{
			"component":  compObjs,
			"population": pops,
			"measureScore": map[string]any{
				"value": b.acc.proportionScore(),
			},
		})
	}
	stratMap["stratum"] = strata
	return stratMap, containedLists
}

func evaluateSupplementalDataObservations(
	spec *measureSpec,
	sdes []measureSupplementalDataSpec,
	outcomes []evaluatedSubjectOutcome,
	effectiveType string,
	targetPatientID string,
) (containedObs []map[string]any, evalRefs []map[string]any) {
	if len(sdes) == 0 || len(outcomes) == 0 {
		return nil, nil
	}

	isIndividual := effectiveType == "individual" && targetPatientID != ""
	for _, sde := range sdes {
		type sdeCountBucket struct {
			cc    canonicalConcept
			count int
		}
		counts := make(map[string]*sdeCountBucket)
		for _, o := range outcomes {
			// For cohort summary / subject-list reports, summarize SDEs over Initial Population subjects.
			// For single-patient individual reports, emit the patient's SDEs even if outside initial-population.
			if !isIndividual && !o.inInitPop {
				continue
			}
			ccs := evaluateConceptsForExpression(o.evalRoot, o.snap, sde.expression)
			for _, cc := range ccs {
				b, exists := counts[cc.key]
				if !exists {
					b = &sdeCountBucket{cc: cc}
					counts[cc.key] = b
				}
				b.count++
			}
		}

		keys := make([]string, 0, len(counts))
		for k := range counts {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		sdeSlug := sanitizeFHIRID(sde.id)
		for idx, k := range keys {
			b := counts[k]
			obsID := fmt.Sprintf("obs-%s-%d", sdeSlug, idx+1)
			obs := map[string]any{
				"resourceType": "Observation",
				"id":           obsID,
				"status":       "final",
				"extension": []any{
					map[string]any{
						"url": "http://hl7.org/fhir/StructureDefinition/cqf-measureInfo",
						"extension": []any{
							map[string]any{
								"url":            "measure",
								"valueCanonical": spec.url,
							},
							map[string]any{
								"url":         "populationId",
								"valueString": sde.id,
							},
						},
					},
				},
				"code":                 sde.code,
				"valueCodeableConcept": b.cc.concept,
				"component": []any{
					map[string]any{
						"code": map[string]any{
							"coding": []any{
								map[string]any{
									"system":  "http://terminology.hl7.org/CodeSystem/measure-data-usage",
									"code":    "supplemental-data",
									"display": "Supplemental Data",
								},
							},
							"text": "count",
						},
						"valueInteger": b.count,
					},
				},
			}
			if isIndividual {
				obs["subject"] = map[string]any{
					"reference": "Patient/" + targetPatientID,
				}
			}
			containedObs = append(containedObs, obs)
			evalRefs = append(evalRefs, map[string]any{
				"reference": "#" + obsID,
				"extension": []any{
					map[string]any{
						"url":         "http://hl7.org/fhir/us/davinci-deqm/StructureDefinition/extension-criteriaReference",
						"valueString": sde.id,
					},
				},
			})
		}
	}
	return containedObs, evalRefs
}
