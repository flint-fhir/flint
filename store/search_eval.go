package store

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/flint-fhir/flint/store/postgres"
)

// IndexedResourceView provides a uniform, zero-copy view of an indexed FHIR resource
// for evaluating advanced FHIR R4 search parameters across Doris and SQLite backends.
type IndexedResourceView struct {
	TenantID      string
	ResType       string
	ResID         string
	ResVersion    int
	LastUpdated   time.Time
	PatientID     string
	EncounterID   string
	ResourceProto []byte
	Indexes       *SearchIndexes
}

// ResourceLookupFunc looks up a single active resource by (tenantID, resType, resID).
type ResourceLookupFunc func(tenantID, resType, resID string) (IndexedResourceView, bool)

// TenantResourcesFunc returns all active resources in tenantID.
type TenantResourcesFunc func(tenantID string) []IndexedResourceView

// EvaluateAdvancedSearchParams evaluates all advanced FHIR R4 search criteria in params
// (Dates, LastUpdated, Quantities with UCUM conversions, :missing, Chained, _has, _filter AST,
// Composite parameters, _content, _text, _list, and Compartments) against view.
func EvaluateAdvancedSearchParams(
	view IndexedResourceView,
	params SearchParams,
	lookup ResourceLookupFunc,
	listTenant TenantResourcesFunc,
) bool {
	tenantID := params.TenantID

	// 1. Compartment filtering
	if params.CompartmentType != "" && params.CompartmentID != "" {
		if !MatchCompartment(tenantID, view, params.CompartmentType, params.CompartmentID, listTenant) {
			return false
		}
	}

	// 2. _lastUpdated filters
	for _, op := range params.LastUpdated {
		if !MatchTimestampOp(view.LastUpdated, op) {
			return false
		}
	}

	// 3. Date & DateFilters
	for spName, op := range params.Dates {
		if !MatchDateParam(view.Indexes, spName, op) {
			return false
		}
	}
	for spName, ops := range params.DateFilters {
		for _, op := range ops {
			if !MatchDateParam(view.Indexes, spName, op) {
				return false
			}
		}
	}

	// 4. Quantity & QuantityFilters (with UCUM unit conversion)
	for spName, op := range params.Quantities {
		if !MatchQuantityParam(view.Indexes, spName, op) {
			return false
		}
	}
	for spName, ops := range params.QuantityFilters {
		for _, op := range ops {
			if !MatchQuantityParam(view.Indexes, spName, op) {
				return false
			}
		}
	}

	// 5. :missing modifier
	for spName, wantMissing := range params.Missing {
		if !MatchMissing(view.ResType, view.Indexes, spName, wantMissing) {
			return false
		}
	}

	// 6. Chained search parameters (single-hop and multi-hop)
	for _, ch := range params.Chained {
		if !MatchChainedParam(tenantID, view.Indexes, ch, lookup, listTenant) {
			return false
		}
	}

	// 7. Reverse chaining (_has, including nested _has)
	for _, hp := range params.Has {
		if !MatchHasParam(tenantID, view.ResType, view.ResID, hp, lookup, listTenant) {
			return false
		}
	}

	// 8. _filter boolean AST expressions
	for _, expr := range params.Filters {
		if !MatchFilterExpr(view, expr, lookup, listTenant) {
			return false
		}
	}

	// 9. Composite search parameters
	if !MatchCompositeParams(view.Indexes, params) {
		return false
	}

	// 10. _content and _text full-text search
	if !MatchContentAndText(view.ResID, view.Indexes, params.Content, params.Text) {
		return false
	}

	// 11. _list cohort search
	if len(params.List) > 0 {
		if !MatchList(tenantID, view.ResType, view.ResID, params.List, lookup) {
			return false
		}
	}

	return true
}

// MatchTimestampOp evaluates a single timestamp against a DateOp interval.
func MatchTimestampOp(ts time.Time, op DateOp) bool {
	return MatchDateRange(ts, ts, op)
}

// MatchDateParam evaluates whether any SpidxDate entry for spName satisfies op.
func MatchDateParam(idx *SearchIndexes, spName string, op DateOp) bool {
	if idx == nil {
		return op.Prefix == "ne"
	}
	if op.Prefix == "ne" {
		for _, d := range idx.Dates {
			if d.SpName == spName && MatchDateRange(d.SpLow, d.SpHigh, DateOp{Prefix: "eq", Value: op.Value, High: op.High}) {
				return false
			}
		}
		return true
	}
	for _, d := range idx.Dates {
		if d.SpName == spName && MatchDateRange(d.SpLow, d.SpHigh, op) {
			return true
		}
	}
	return false
}

// MatchDateRange evaluates a [spLow, spHigh] interval against a FHIR DateOp.
func MatchDateRange(spLow, spHigh time.Time, op DateOp) bool {
	low := op.Value
	high := op.High
	if high.IsZero() || high.Before(low) {
		high = low.Add(24 * time.Hour)
	}
	if spHigh.IsZero() || spHigh.Before(spLow) {
		spHigh = spLow
	}

	switch op.Prefix {
	case "", "eq":
		// Overlap between [spLow, spHigh] and [low, high)
		if spHigh.Equal(spLow) {
			return !spLow.Before(low) && spLow.Before(high)
		}
		return !spLow.After(high.Add(-time.Nanosecond)) && !spHigh.Before(low)
	case "ne":
		if spHigh.Equal(spLow) {
			return spLow.Before(low) || !spLow.Before(high)
		}
		return spHigh.Before(low) || !spLow.Before(high)
	case "gt":
		return !spHigh.Before(high)
	case "lt":
		return spLow.Before(low)
	case "ge":
		return !spHigh.Before(low)
	case "le":
		return spLow.Before(high)
	case "sa":
		return !spLow.Before(high)
	case "eb":
		return spHigh.Before(low)
	case "ap":
		span := high.Sub(low)
		if span < 24*time.Hour {
			span = 24 * time.Hour
		}
		apLow := low.Add(-span)
		apHigh := high.Add(span)
		return !spLow.After(apHigh) && !spHigh.Before(apLow)
	default:
		return !spLow.Before(low) && spLow.Before(high)
	}
}

type ucumFactor struct {
	dim    string
	factor float64
}

var ucumConversionTable = map[string]ucumFactor{
	// Mass (canonical: g)
	"kg":      {dim: "mass", factor: 1000.0},
	"g":       {dim: "mass", factor: 1.0},
	"mg":      {dim: "mass", factor: 0.001},
	"ug":      {dim: "mass", factor: 1e-6},
	"mcg":     {dim: "mass", factor: 1e-6},
	"ng":      {dim: "mass", factor: 1e-9},
	"pg":      {dim: "mass", factor: 1e-12},
	"[lb_av]": {dim: "mass", factor: 453.59237},
	"lb":      {dim: "mass", factor: 453.59237},
	"[oz_av]": {dim: "mass", factor: 28.349523125},
	"oz":      {dim: "mass", factor: 28.349523125},

	// Length (canonical: m)
	"km":     {dim: "length", factor: 1000.0},
	"m":      {dim: "length", factor: 1.0},
	"cm":     {dim: "length", factor: 0.01},
	"mm":     {dim: "length", factor: 0.001},
	"um":     {dim: "length", factor: 1e-6},
	"nm":     {dim: "length", factor: 1e-9},
	"[in_i]": {dim: "length", factor: 0.0254},
	"in":     {dim: "length", factor: 0.0254},
	"[ft_i]": {dim: "length", factor: 0.3048},
	"ft":     {dim: "length", factor: 0.3048},

	// Volume (canonical: L)
	"L":        {dim: "volume", factor: 1.0},
	"l":        {dim: "volume", factor: 1.0},
	"dL":       {dim: "volume", factor: 0.1},
	"dl":       {dim: "volume", factor: 0.1},
	"mL":       {dim: "volume", factor: 0.001},
	"ml":       {dim: "volume", factor: 0.001},
	"uL":       {dim: "volume", factor: 1e-6},
	"ul":       {dim: "volume", factor: 1e-6},
	"[foz_us]": {dim: "volume", factor: 0.0295735295625},
	"[gal_us]": {dim: "volume", factor: 3.785411784},

	// Duration (canonical: s)
	"s":   {dim: "time", factor: 1.0},
	"ms":  {dim: "time", factor: 0.001},
	"min": {dim: "time", factor: 60.0},
	"h":   {dim: "time", factor: 3600.0},
	"hr":  {dim: "time", factor: 3600.0},
	"d":   {dim: "time", factor: 86400.0},
	"wk":  {dim: "time", factor: 604800.0},
	"mo":  {dim: "time", factor: 2629746.0},
	"a":   {dim: "time", factor: 31556952.0},
	"yr":  {dim: "time", factor: 31556952.0},

	// Mass Concentration (canonical: g/L)
	"g/L":   {dim: "mass_conc", factor: 1.0},
	"g/dL":  {dim: "mass_conc", factor: 10.0},
	"mg/L":  {dim: "mass_conc", factor: 0.001},
	"mg/dL": {dim: "mass_conc", factor: 0.01},
	"mg/mL": {dim: "mass_conc", factor: 1.0},
	"ug/L":  {dim: "mass_conc", factor: 1e-6},
	"ug/dL": {dim: "mass_conc", factor: 1e-5},
	"ug/mL": {dim: "mass_conc", factor: 0.001},
	"ng/mL": {dim: "mass_conc", factor: 1e-6},

	// Substance Concentration (canonical: mol/L)
	"mol/L":  {dim: "subst_conc", factor: 1.0},
	"mmol/L": {dim: "subst_conc", factor: 0.001},
	"umol/L": {dim: "subst_conc", factor: 1e-6},
	"nmol/L": {dim: "subst_conc", factor: 1e-9},
	"pmol/L": {dim: "subst_conc", factor: 1e-12},
}

func isTempUnit(u string) bool {
	switch u {
	case "Cel", "C", "°C", "[degF]", "F", "°F", "K":
		return true
	default:
		return false
	}
}

func tempToCelsius(val float64, u string) float64 {
	switch u {
	case "[degF]", "F", "°F":
		return (val - 32.0) * 5.0 / 9.0
	case "K":
		return val - 273.15
	default:
		return val
	}
}

// ConvertUCUMValue converts qVal in qCode into the target unit opCode when both belong to the
// same UCUM dimension. Returns (convertedVal, true) if compatible, or (0, false) if incompatible.
func ConvertUCUMValue(qVal float64, qCode, opCode string) (float64, bool) {
	if opCode == "" || qCode == opCode || strings.EqualFold(qCode, opCode) {
		return qVal, true
	}
	if isTempUnit(qCode) && isTempUnit(opCode) {
		cel := tempToCelsius(qVal, qCode)
		switch opCode {
		case "[degF]", "F", "°F":
			return cel*9.0/5.0 + 32.0, true
		case "K":
			return cel + 273.15, true
		default:
			return cel, true
		}
	}
	fQ, okQ := ucumConversionTable[qCode]
	fOp, okOp := ucumConversionTable[opCode]
	if okQ && okOp && fQ.dim == fOp.dim && fOp.factor != 0 {
		return qVal * (fQ.factor / fOp.factor), true
	}
	return 0, false
}

// MatchQuantityParam evaluates whether any SpidxQuantity entry for spName satisfies op.
func MatchQuantityParam(idx *SearchIndexes, spName string, op QuantityOp) bool {
	if idx == nil {
		return op.Prefix == "ne"
	}
	if op.Prefix == "ne" {
		for _, q := range idx.Quantities {
			if q.SpName == spName && MatchSingleQuantity(q, QuantityOp{
				Prefix: "eq",
				Value:  op.Value,
				System: op.System,
				Code:   op.Code,
			}) {
				return false
			}
		}
		return true
	}
	for _, q := range idx.Quantities {
		if q.SpName == spName && MatchSingleQuantity(q, op) {
			return true
		}
	}
	return false
}

// MatchSingleQuantity evaluates a single SpidxQuantity against a QuantityOp (including UCUM conversion).
func MatchSingleQuantity(q SpidxQuantity, op QuantityOp) bool {
	isUCUM := (op.System == "" || op.System == "http://unitsofmeasure.org") &&
		(q.SpSystem == "" || q.SpSystem == "http://unitsofmeasure.org")
	if op.System != "" && q.SpSystem != op.System && !isUCUM {
		return false
	}
	val := q.SpValue
	if op.Code != "" {
		if isUCUM {
			conv, ok := ConvertUCUMValue(q.SpValue, q.SpCode, op.Code)
			if !ok {
				return false
			}
			val = conv
		} else if q.SpCode != op.Code {
			return false
		}
	}
	const eps = 1e-6
	switch op.Prefix {
	case "", "eq":
		return math.Abs(val-op.Value) <= eps
	case "ne":
		return math.Abs(val-op.Value) > eps
	case "gt":
		return val > op.Value+eps
	case "lt":
		return val < op.Value-eps
	case "ge":
		return val >= op.Value-eps
	case "le":
		return val <= op.Value+eps
	case "ap":
		tol := math.Max(math.Abs(op.Value)*0.10, eps)
		return math.Abs(val-op.Value) <= tol
	default:
		return math.Abs(val-op.Value) <= eps
	}
}

// SearchParamCategory returns the primary search parameter category ("string", "token", "date", "quantity", "reference", "uri")
// for a FHIR R4 search parameter name.
func SearchParamCategory(spName string) string {
	base, mod, _ := strings.Cut(spName, ":")
	if mod == "text" {
		return "string"
	}
	switch base {
	case "_lastUpdated", "birthdate", "date", "onset-date", "authoredon", "authored", "issued", "last-date",
		"collected", "period", "recorded", "created", "indexed", "target-date", "start-date",
		"occurrence", "whenhandedover", "whenprepared", "value-date":
		return "date"
	case "value-quantity", "component-value-quantity", "combo-value-quantity":
		return "quantity"
	case "family", "given", "name", "address", "address-city", "address-state",
		"address-postalcode", "address-country", "phonetic", "lot-number", "series",
		"title", "description", "udi-carrier", "device-name", "value-string":
		return "string"
	case "organization", "general-practitioner", "subject", "patient", "beneficiary", "encounter",
		"performer", "asserter", "recorder", "participant", "requester", "medication",
		"intended-dispenser", "intended-performer", "based-on", "location", "part-of", "partof",
		"reason-reference", "result", "results-interpreter", "specimen", "collector", "media",
		"manufacturer", "reaction", "endpoint", "instantiates-canonical", "target", "agent",
		"author", "custodian", "payor", "subscriber", "practitioner", "device", "questionnaire",
		"source", "care-team", "goal", "focus", "owner", "for", "actor", "item", "member",
		"entity", "service-provider":
		return "reference"
	case "url", "system", "source-uri", "target-uri", "instantiates-uri", "definition", "_profile", "_source":
		return "uri"
	case "identifier", "gender", "active", "email", "phone", "telecom", "language",
		"deceased", "status", "code", "component-code", "combo-code", "category", "type",
		"class", "clinical-status", "verification-status", "lifecycle-status", "docstatus",
		"mode", "intent", "priority", "criticality", "severity", "manifestation", "route",
		"reason-code", "conclusion", "vaccine-code", "status-reason", "target-disease",
		"intended-performertype", "performer-type", "address-use", "telecom-use", "accession",
		"bodysite", "specialty", "role", "relationship", "subscriber-id", "value-concept",
		"component-value-concept", "combo-value-concept", "race", "ethnicity", "birthsex",
		"marital-status", "_tag", "_security":
		return "token"
	default:
		return ""
	}
}

// MatchMissing evaluates a :missing=true or :missing=false predicate against idx.
func MatchMissing(resType string, idx *SearchIndexes, spName string, wantMissing bool) bool {
	if idx == nil {
		return wantMissing
	}
	_ = resType
	present := hasAnySpidxName(idx, spName)
	if wantMissing {
		return !present
	}
	return present
}

func hasAnySpidxName(idx *SearchIndexes, spName string) bool {
	for _, s := range idx.Strings {
		if s.SpName == spName {
			return true
		}
	}
	for _, t := range idx.Tokens {
		if t.SpName == spName {
			return true
		}
	}
	for _, d := range idx.Dates {
		if d.SpName == spName {
			return true
		}
	}
	for _, r := range idx.References {
		if r.SpName == spName {
			return true
		}
	}
	for _, q := range idx.Quantities {
		if q.SpName == spName {
			return true
		}
	}
	for _, u := range idx.URIs {
		if u.SpName == spName {
			return true
		}
	}
	return false
}

// MatchChainedParam evaluates single-hop and multi-hop chained search parameters
// (e.g. subject:Patient.name=Smith or subject:Patient.organization:Organization.name=General).
func MatchChainedParam(
	tenantID string,
	idx *SearchIndexes,
	ch ChainedParam,
	lookup ResourceLookupFunc,
	listTenant TenantResourcesFunc,
) bool {
	if idx == nil {
		return false
	}
	hops := ch.Hops
	if len(hops) == 0 {
		hops = []ChainedHop{{
			RefParam:   ch.RefParam,
			TargetType: ch.TargetType,
		}}
	}
	return matchChainedHops(tenantID, idx, hops, ch, lookup, listTenant)
}

func matchChainedHops(
	tenantID string,
	idx *SearchIndexes,
	hops []ChainedHop,
	ch ChainedParam,
	lookup ResourceLookupFunc,
	listTenant TenantResourcesFunc,
) bool {
	if idx == nil || len(hops) == 0 {
		return false
	}
	hop := hops[0]
	for _, ref := range idx.References {
		if ref.SpName != hop.RefParam {
			continue
		}
		if hop.TargetType != "" && ref.TargetType != "" && !strings.EqualFold(hop.TargetType, ref.TargetType) {
			continue
		}
		targets := resolveReferencedTargets(tenantID, hop.TargetType, ref.TargetType, ref.TargetID, lookup, listTenant)
		for _, target := range targets {
			if len(hops) > 1 {
				if matchChainedHops(tenantID, target.Indexes, hops[1:], ch, lookup, listTenant) {
					return true
				}
			} else if matchChainedLeaf(tenantID, target, ch, lookup, listTenant) {
				return true
			}
		}
	}
	return false
}

func resolveReferencedTargets(
	tenantID, wantType, refTargetType, refTargetID string,
	lookup ResourceLookupFunc,
	listTenant TenantResourcesFunc,
) []IndexedResourceView {
	if refTargetID == "" {
		return nil
	}
	effectiveType := wantType
	if effectiveType == "" {
		effectiveType = refTargetType
	}
	if effectiveType != "" && lookup != nil {
		if v, ok := lookup(tenantID, effectiveType, refTargetID); ok {
			return []IndexedResourceView{v}
		}
		return nil
	}
	if listTenant == nil {
		return nil
	}
	var out []IndexedResourceView
	for _, v := range listTenant(tenantID) {
		if v.ResID == refTargetID && (effectiveType == "" || strings.EqualFold(v.ResType, effectiveType)) {
			out = append(out, v)
		}
	}
	return out
}

func matchChainedLeaf(
	tenantID string,
	target IndexedResourceView,
	ch ChainedParam,
	lookup ResourceLookupFunc,
	listTenant TenantResourcesFunc,
) bool {
	targetBase, targetMod, _ := strings.Cut(ch.TargetParam, ":")
	if ch.TargetModifier != "" {
		targetMod = ch.TargetModifier
	}
	spKey := targetBase
	if targetMod != "" {
		spKey = targetBase + ":" + targetMod
	}
	paramType := ch.ParamType
	if targetBase == "_lastUpdated" {
		paramType = "date"
	} else if paramType == "" {
		if targetBase == "_id" {
			paramType = "id"
		} else {
			paramType = SearchParamCategory(spKey)
		}
	}
	switch paramType {
	case "id":
		return matchIDCSV(target.ResID, ch.Value)
	case "string":
		return MatchIndexString(target.Indexes, spKey, ch.Value)
	case "token":
		return MatchIndexToken(target.Indexes, spKey, ch.Value)
	case "date":
		dOp, err := ParseDateOp(ch.Value)
		if err != nil {
			return false
		}
		if targetBase == "_lastUpdated" {
			return MatchTimestampOp(target.LastUpdated, dOp)
		}
		return MatchDateParam(target.Indexes, targetBase, dOp)
	case "quantity":
		qOp, err := ParseQuantityOp(ch.Value)
		if err != nil {
			return false
		}
		return MatchQuantityParam(target.Indexes, targetBase, qOp)
	case "uri":
		return MatchIndexURI(target.Indexes, spKey, ch.Value)
	case "reference":
		return MatchIndexReference(tenantID, target.Indexes, spKey, ch.Value, lookup, listTenant)
	default:
		return MatchIndexString(target.Indexes, spKey, ch.Value) || MatchIndexToken(target.Indexes, spKey, ch.Value)
	}
}

func matchIDCSV(resID, raw string) bool {
	for part := range strings.SplitSeq(raw, ",") {
		cleaned := strings.TrimSpace(part)
		if slash := strings.LastIndexByte(cleaned, '/'); slash != -1 {
			cleaned = cleaned[slash+1:]
		}
		if cleaned == resID {
			return true
		}
	}
	return false
}

// MatchIndexString evaluates a FHIR string search parameter (including :exact, :contains, :text) against idx.
func MatchIndexString(idx *SearchIndexes, spName, rawStr string) bool {
	if idx == nil {
		return false
	}
	baseName := strings.Split(spName, ":")[0]
	exact := strings.HasSuffix(spName, ":exact")
	contains := strings.HasSuffix(spName, ":contains") || strings.HasSuffix(spName, ":text")
	isText := strings.HasSuffix(spName, ":text")
	for item := range strings.SplitSeq(rawStr, ",") {
		needle := strings.ToLower(strings.TrimSpace(item))
		if exact {
			needle = strings.TrimSpace(item)
		}
		for _, st := range idx.Strings {
			if st.SpName != baseName && (!isText || st.SpName != baseName+":text") {
				continue
			}
			if exact {
				if st.SpValue == needle || strings.EqualFold(st.SpValue, needle) {
					return true
				}
				continue
			}
			val := strings.ToLower(strings.TrimSpace(st.SpValue))
			if contains && strings.Contains(val, needle) {
				return true
			}
			if !contains && strings.HasPrefix(val, needle) {
				return true
			}
		}
	}
	return false
}

// MatchIndexToken evaluates a FHIR token search parameter (including :not, :of-type, :text) against idx.
func MatchIndexToken(idx *SearchIndexes, spName, rawVal string) bool {
	if strings.HasSuffix(spName, ":text") {
		return MatchIndexString(idx, spName, rawVal)
	}
	baseName := spName
	negate := false
	if strings.HasSuffix(baseName, ":not") {
		baseName = strings.TrimSuffix(baseName, ":not")
		negate = true
	}
	isOfType := strings.HasSuffix(baseName, ":of-type")
	matched := false
	if idx != nil {
		for item := range strings.SplitSeq(rawVal, ",") {
			item = strings.TrimSpace(item)
			if isOfType {
				if parts := strings.SplitN(item, "|", 3); len(parts) == 3 {
					sys := parts[0] + "|" + parts[1]
					code := parts[2]
					for _, t := range idx.Tokens {
						if t.SpName == baseName && t.SpSystem == sys && t.SpValue == code {
							matched = true
							break
						}
					}
					if matched {
						break
					}
				}
				continue
			}
			sys := ""
			code := item
			hasSys := false
			if parts := strings.SplitN(item, "|", 2); len(parts) == 2 {
				sys, code = parts[0], parts[1]
				hasSys = true
			}
			for _, t := range idx.Tokens {
				if t.SpName != baseName {
					continue
				}
				if hasSys {
					if code == "" {
						if t.SpSystem == sys {
							matched = true
							break
						}
					} else if t.SpSystem == sys && t.SpValue == code {
						matched = true
						break
					}
				} else if strings.EqualFold(t.SpValue, code) {
					matched = true
					break
				}
			}
			if matched {
				break
			}
		}
	}
	if negate {
		return !matched
	}
	return matched
}

// MatchIndexURI evaluates a FHIR URI search parameter (including :below and :above) against idx.
func MatchIndexURI(idx *SearchIndexes, spName, rawURI string) bool {
	if idx == nil {
		return false
	}
	baseName, mod, _ := strings.Cut(spName, ":")
	for item := range strings.SplitSeq(rawURI, ",") {
		want := strings.TrimSpace(item)
		for _, u := range idx.URIs {
			if u.SpName != baseName {
				continue
			}
			switch mod {
			case "below":
				if strings.HasPrefix(u.SpValue, strings.TrimSuffix(want, "/")) {
					return true
				}
			case "above":
				if strings.HasPrefix(strings.TrimSuffix(want, "/"), strings.TrimSuffix(u.SpValue, "/")) {
					return true
				}
			default:
				if u.SpValue == want {
					return true
				}
			}
		}
	}
	return false
}

// MatchIndexReference evaluates a FHIR reference search parameter (including :identifier) against idx.
func MatchIndexReference(
	tenantID string,
	idx *SearchIndexes,
	spName, rawRef string,
	lookup ResourceLookupFunc,
	listTenant TenantResourcesFunc,
) bool {
	if idx == nil {
		return false
	}
	if strings.HasSuffix(spName, ":identifier") {
		if MatchIndexToken(idx, spName, rawRef) {
			return true
		}
		baseName := strings.TrimSuffix(spName, ":identifier")
		for _, r := range idx.References {
			if r.SpName != baseName {
				continue
			}
			for _, target := range resolveReferencedTargets(tenantID, "", r.TargetType, r.TargetID, lookup, listTenant) {
				if MatchIndexToken(target.Indexes, "identifier", rawRef) {
					return true
				}
			}
		}
		return false
	}
	baseName, wantType, hasTypeMod := strings.Cut(spName, ":")
	for item := range strings.SplitSeq(rawRef, ",") {
		item = strings.TrimSpace(item)
		targetID := item
		var itemType string
		if slash := strings.LastIndexByte(item, '/'); slash != -1 {
			prefix := item[:slash]
			if slash2 := strings.LastIndexByte(prefix, '/'); slash2 != -1 {
				itemType = prefix[slash2+1:]
			} else {
				itemType = prefix
			}
			targetID = item[slash+1:]
		}
		if hasTypeMod && wantType != "" && itemType == "" {
			itemType = wantType
		}
		for _, r := range idx.References {
			if r.SpName != baseName {
				continue
			}
			if itemType != "" && r.TargetType != "" && !strings.EqualFold(r.TargetType, itemType) {
				continue
			}
			if r.TargetID == targetID {
				return true
			}
		}
	}
	return false
}

// MatchHasParam evaluates a reverse-chaining _has parameter (including nested _has Child chains).
func MatchHasParam(
	tenantID, resType, resID string,
	hp HasParam,
	lookup ResourceLookupFunc,
	listTenant TenantResourcesFunc,
) bool {
	if listTenant == nil {
		return false
	}
	for _, cand := range listTenant(tenantID) {
		if hp.SourceType != "" && !strings.EqualFold(cand.ResType, hp.SourceType) {
			continue
		}
		if cand.Indexes == nil {
			continue
		}
		linksToTarget := false
		for _, ref := range cand.Indexes.References {
			if ref.SpName != hp.RefParam {
				continue
			}
			if ref.TargetID == resID && (ref.TargetType == "" || resType == "" || strings.EqualFold(ref.TargetType, resType)) {
				linksToTarget = true
				break
			}
		}
		if !linksToTarget {
			continue
		}
		if hp.Child != nil {
			if MatchHasParam(tenantID, cand.ResType, cand.ResID, *hp.Child, lookup, listTenant) {
				return true
			}
			continue
		}
		searchBase, searchMod, _ := strings.Cut(hp.SearchParam, ":")
		leafCh := ChainedParam{
			TargetParam:    searchBase,
			TargetModifier: searchMod,
			ParamType:      hp.ParamType,
			Value:          hp.Value,
		}
		if matchChainedLeaf(tenantID, cand, leafCh, lookup, listTenant) {
			return true
		}
	}
	return false
}

// MatchFilterExpr evaluates a RFC-style FHIR _filter boolean AST expression against view.
func MatchFilterExpr(
	view IndexedResourceView,
	expr FilterExpr,
	lookup ResourceLookupFunc,
	listTenant TenantResourcesFunc,
) bool {
	switch expr.Kind {
	case "and":
		for _, ch := range expr.Children {
			if !MatchFilterExpr(view, ch, lookup, listTenant) {
				return false
			}
		}
		return true
	case "or":
		for _, ch := range expr.Children {
			if MatchFilterExpr(view, ch, lookup, listTenant) {
				return true
			}
		}
		return false
	case "not":
		if len(expr.Children) == 0 {
			return true
		}
		return !MatchFilterExpr(view, expr.Children[0], lookup, listTenant)
	case "param":
		return matchFilterAtom(view, expr, lookup, listTenant)
	default:
		return true
	}
}

func matchFilterAtom(
	view IndexedResourceView,
	expr FilterExpr,
	lookup ResourceLookupFunc,
	listTenant TenantResourcesFunc,
) bool {
	param := strings.TrimSpace(expr.Param)
	op := strings.ToLower(strings.TrimSpace(expr.Op))
	val := strings.TrimSpace(expr.Value)

	if strings.Contains(param, ".") {
		if ch, ok := ParseChainedParam(param, mapFilterOpToValue(op, val)); ok && ch != nil {
			if op == "ne" {
				return !MatchChainedParam(view.TenantID, view.Indexes, *ch, lookup, listTenant)
			}
			return MatchChainedParam(view.TenantID, view.Indexes, *ch, lookup, listTenant)
		}
	}

	if op == "pr" {
		wantPresent := strings.EqualFold(val, "true")
		return MatchMissing(view.ResType, view.Indexes, param, !wantPresent)
	}

	if param == "_id" {
		matched := matchIDCSV(view.ResID, val)
		if op == "ne" {
			return !matched
		}
		return matched
	}
	if param == "_lastUpdated" {
		dOp, err := ParseDateOp(mapDateFilterPrefix(op) + val)
		if err != nil {
			return false
		}
		return MatchTimestampOp(view.LastUpdated, dOp)
	}

	cat := SearchParamCategory(param)
	switch cat {
	case "date":
		dOp, err := ParseDateOp(mapDateFilterPrefix(op) + val)
		if err != nil {
			return false
		}
		return MatchDateParam(view.Indexes, param, dOp)
	case "quantity":
		qOp, err := ParseQuantityOp(mapQuantityFilterPrefix(op) + val)
		if err != nil {
			return false
		}
		return MatchQuantityParam(view.Indexes, param, qOp)
	case "string":
		return matchFilterStringOp(view.Indexes, param, op, val)
	case "token":
		if op == "ne" {
			return MatchIndexToken(view.Indexes, param+":not", val)
		}
		if op == "co" {
			return MatchIndexString(view.Indexes, param+":text", val)
		}
		return MatchIndexToken(view.Indexes, param, val)
	case "reference":
		matched := MatchIndexReference(view.TenantID, view.Indexes, param, val, lookup, listTenant)
		if op == "ne" {
			return !matched
		}
		return matched
	case "uri":
		spKey := param
		switch op {
		case "sb":
			spKey = param + ":below"
		case "ss":
			spKey = param + ":above"
		}
		matched := MatchIndexURI(view.Indexes, spKey, val)
		if op == "ne" {
			return !matched
		}
		return matched
	default:
		if op == "ne" {
			return !MatchIndexString(view.Indexes, param, val) && MatchIndexToken(view.Indexes, param+":not", val)
		}
		return matchFilterStringOp(view.Indexes, param, op, val) || MatchIndexToken(view.Indexes, param, val)
	}
}

func mapFilterOpToValue(op, val string) string {
	switch op {
	case "gt", "lt", "ge", "le", "sa", "eb", "ap", "ne":
		return op + val
	default:
		return val
	}
}

func mapDateFilterPrefix(op string) string {
	switch op {
	case "eq", "ne", "gt", "lt", "ge", "le", "sa", "eb", "ap":
		return op
	default:
		return "eq"
	}
}

func mapQuantityFilterPrefix(op string) string {
	switch op {
	case "eq", "ne", "gt", "lt", "ge", "le", "ap":
		return op
	default:
		return "eq"
	}
}

func matchFilterStringOp(idx *SearchIndexes, spName, op, val string) bool {
	if idx == nil {
		return op == "ne"
	}
	needle := strings.ToLower(strings.TrimSpace(val))
	for _, st := range idx.Strings {
		if st.SpName != spName {
			continue
		}
		cand := strings.ToLower(strings.TrimSpace(st.SpValue))
		switch op {
		case "eq":
			if cand == needle || strings.HasPrefix(cand, needle) {
				return true
			}
		case "ne":
			if cand == needle || strings.HasPrefix(cand, needle) {
				return false
			}
		case "co":
			if strings.Contains(cand, needle) {
				return true
			}
		case "sw":
			if strings.HasPrefix(cand, needle) {
				return true
			}
		case "ew":
			if strings.HasSuffix(cand, needle) {
				return true
			}
		}
	}
	return op == "ne"
}

// MatchCompositeParams evaluates all 4 composite search parameter families against idx.
func MatchCompositeParams(idx *SearchIndexes, params SearchParams) bool {
	if len(params.CompositeTokenQuantities) == 0 &&
		len(params.CompositeTokenTokens) == 0 &&
		len(params.CompositeTokenDates) == 0 &&
		len(params.CompositeTokenStrings) == 0 {
		return true
	}
	if idx == nil {
		return false
	}

	for _, orGroup := range params.CompositeTokenQuantities {
		groupMatched := false
		for _, cp := range orGroup {
			if matchCompositeTokenQuantityItem(idx, cp) {
				groupMatched = true
				break
			}
		}
		if !groupMatched {
			return false
		}
	}

	for _, orGroup := range params.CompositeTokenTokens {
		groupMatched := false
		for _, cp := range orGroup {
			if matchCompositeTokenTokenItem(idx, cp) {
				groupMatched = true
				break
			}
		}
		if !groupMatched {
			return false
		}
	}

	for _, orGroup := range params.CompositeTokenDates {
		groupMatched := false
		for _, cp := range orGroup {
			if MatchIndexToken(idx, "code", formatSysCode(cp.TokenSystem, cp.TokenCode)) &&
				MatchDateParam(idx, "value-date", cp.Date) {
				groupMatched = true
				break
			}
		}
		if !groupMatched {
			return false
		}
	}

	for _, orGroup := range params.CompositeTokenStrings {
		groupMatched := false
		for _, cp := range orGroup {
			if MatchIndexToken(idx, "code", formatSysCode(cp.TokenSystem, cp.TokenCode)) &&
				MatchIndexString(idx, "value-string", cp.Value) {
				groupMatched = true
				break
			}
		}
		if !groupMatched {
			return false
		}
	}

	return true
}

func matchCompositeTokenQuantityItem(idx *SearchIndexes, cp CompositeTokenQuantityParam) bool {
	compKey := "comp-cvq:" + cp.TokenCode
	if cp.TokenSystem != "" {
		compKey = "comp-cvq:" + cp.TokenSystem + "|" + cp.TokenCode
	}
	matchComponent := func() bool {
		if MatchQuantityParam(idx, compKey, cp.Quantity) {
			return true
		}
		if cp.TokenSystem == "" {
			suffix := "|" + cp.TokenCode
			for _, q := range idx.Quantities {
				if strings.HasPrefix(q.SpName, "comp-cvq:") &&
					(q.SpName == "comp-cvq:"+cp.TokenCode || strings.HasSuffix(q.SpName, suffix)) &&
					MatchSingleQuantity(q, cp.Quantity) {
					return true
				}
			}
		}
		return false
	}
	matchTopLevel := func() bool {
		return MatchIndexToken(idx, "code", formatSysCode(cp.TokenSystem, cp.TokenCode)) &&
			MatchQuantityParam(idx, "value-quantity", cp.Quantity)
	}
	switch cp.ParamName {
	case "component-code-value-quantity":
		return matchComponent()
	case "combo-code-value-quantity":
		return matchComponent() || matchTopLevel()
	default:
		return matchTopLevel()
	}
}

func matchCompositeTokenTokenItem(idx *SearchIndexes, cp CompositeTokenTokenParam) bool {
	compKey := "comp-cvc:" + cp.TokenCode
	if cp.TokenSystem != "" {
		compKey = "comp-cvc:" + cp.TokenSystem + "|" + cp.TokenCode
	}
	valToken := formatSysCode(cp.ValueSystem, cp.ValueCode)
	matchComponent := func() bool {
		if MatchIndexToken(idx, compKey, valToken) {
			return true
		}
		if cp.TokenSystem == "" {
			suffix := "|" + cp.TokenCode
			for _, t := range idx.Tokens {
				if strings.HasPrefix(t.SpName, "comp-cvc:") &&
					(t.SpName == "comp-cvc:"+cp.TokenCode || strings.HasSuffix(t.SpName, suffix)) &&
					matchSingleTokenRow(t, cp.ValueSystem, cp.ValueCode) {
					return true
				}
			}
		}
		return false
	}
	matchTopLevel := func() bool {
		return MatchIndexToken(idx, "code", formatSysCode(cp.TokenSystem, cp.TokenCode)) &&
			MatchIndexToken(idx, "value-concept", valToken)
	}
	switch cp.ParamName {
	case "component-code-value-concept":
		return matchComponent()
	case "combo-code-value-concept":
		return matchComponent() || matchTopLevel()
	default:
		return matchTopLevel()
	}
}

func matchSingleTokenRow(t SpidxToken, sys, code string) bool {
	if sys != "" && t.SpSystem != sys {
		return false
	}
	return strings.EqualFold(t.SpValue, code)
}

func formatSysCode(sys, code string) string {
	if sys != "" {
		return sys + "|" + code
	}
	return code
}

// MatchContentAndText evaluates _content and _text search terms against the resource's indexed strings and tokens.
func MatchContentAndText(resID string, idx *SearchIndexes, contentTerms, textTerms []string) bool {
	for _, rawTerm := range contentTerms {
		matchedAny := false
		for item := range strings.SplitSeq(rawTerm, ",") {
			needle := strings.ToLower(strings.TrimSpace(item))
			if needle == "" {
				continue
			}
			if strings.Contains(strings.ToLower(resID), needle) || indexContainsSubstring(idx, needle, false) {
				matchedAny = true
				break
			}
		}
		if !matchedAny {
			return false
		}
	}
	for _, rawTerm := range textTerms {
		matchedAny := false
		for item := range strings.SplitSeq(rawTerm, ",") {
			needle := strings.ToLower(strings.TrimSpace(item))
			if needle == "" {
				continue
			}
			if indexContainsSubstring(idx, needle, true) {
				matchedAny = true
				break
			}
		}
		if !matchedAny {
			return false
		}
	}
	return true
}

func indexContainsSubstring(idx *SearchIndexes, needle string, preferText bool) bool {
	if idx == nil {
		return false
	}
	for _, s := range idx.Strings {
		if preferText && s.SpName != "_text" && !strings.HasSuffix(s.SpName, ":text") {
			continue
		}
		if strings.Contains(strings.ToLower(s.SpValue), needle) {
			return true
		}
	}
	if preferText {
		for _, s := range idx.Strings {
			if strings.Contains(strings.ToLower(s.SpValue), needle) {
				return true
			}
		}
	}
	for _, t := range idx.Tokens {
		if strings.Contains(strings.ToLower(t.SpValue), needle) || strings.Contains(strings.ToLower(t.SpSystem), needle) {
			return true
		}
	}
	for _, u := range idx.URIs {
		if strings.Contains(strings.ToLower(u.SpValue), needle) {
			return true
		}
	}
	return false
}

// MatchList evaluates whether (resType, resID) is referenced as an item/member in all specified List or Group resources.
func MatchList(tenantID, resType, resID string, listIDs []string, lookup ResourceLookupFunc) bool {
	if lookup == nil {
		return false
	}
	for _, rawList := range listIDs {
		matchedOne := false
		for item := range strings.SplitSeq(rawList, ",") {
			listID := strings.TrimSpace(item)
			if slash := strings.LastIndexByte(listID, '/'); slash != -1 {
				listID = listID[slash+1:]
			}
			if listID == "" {
				continue
			}
			for _, containerType := range []string{"List", "Group"} {
				listView, ok := lookup(tenantID, containerType, listID)
				if !ok || listView.Indexes == nil {
					continue
				}
				for _, ref := range listView.Indexes.References {
					if (ref.SpName == "item" || ref.SpName == "member" || ref.SpName == "entity") &&
						ref.TargetID == resID &&
						(ref.TargetType == "" || strings.EqualFold(ref.TargetType, resType)) {
						matchedOne = true
						break
					}
				}
				if matchedOne {
					break
				}
			}
			if matchedOne {
				break
			}
		}
		if !matchedOne {
			return false
		}
	}
	return true
}

// MatchCompartment evaluates whether view belongs to the FHIR R4 compartment (compType, compID).
func MatchCompartment(
	tenantID string,
	view IndexedResourceView,
	compType, compID string,
	listTenant TenantResourcesFunc,
) bool {
	canonType := postgres.CanonicalCompartmentType(compType)
	if canonType == "" {
		return true
	}
	compID = strings.TrimSpace(compID)
	if slash := strings.LastIndexByte(compID, '/'); slash != -1 {
		compID = compID[slash+1:]
	}
	if strings.EqualFold(view.ResType, canonType) && view.ResID == compID {
		return true
	}
	switch canonType {
	case "Patient":
		if view.PatientID == compID {
			return true
		}
	case "Encounter":
		if view.EncounterID == compID {
			return true
		}
	}
	if view.Indexes != nil {
		for _, ref := range view.Indexes.References {
			if ref.TargetID == compID && (ref.TargetType == "" || strings.EqualFold(ref.TargetType, canonType)) {
				return true
			}
		}
		if strings.EqualFold(view.ResType, "Provenance") && listTenant != nil {
			for _, ref := range view.Indexes.References {
				if ref.SpName != "target" {
					continue
				}
				for _, cand := range listTenant(tenantID) {
					if cand.ResID == ref.TargetID && (ref.TargetType == "" || strings.EqualFold(cand.ResType, ref.TargetType)) {
						if MatchCompartment(tenantID, cand, canonType, compID, nil) {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

// SortSearchResults sorts matches according to sorts (or defaults to _lastUpdated DESC, _id ASC).
func SortSearchResults(tenantID string, matches []SearchResult, sorts []SortSpec, lookup ResourceLookupFunc) {
	if len(sorts) == 0 {
		sort.Slice(matches, func(i, j int) bool {
			if !matches[i].LastUpdated.Equal(matches[j].LastUpdated) {
				return matches[i].LastUpdated.After(matches[j].LastUpdated)
			}
			return matches[i].ResID < matches[j].ResID
		})
		return
	}
	sort.SliceStable(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		for _, s := range sorts {
			cmp := compareBySortSpec(tenantID, a, b, s, lookup)
			if cmp != 0 {
				if s.Desc {
					return cmp > 0
				}
				return cmp < 0
			}
		}
		if !a.LastUpdated.Equal(b.LastUpdated) {
			return a.LastUpdated.After(b.LastUpdated)
		}
		return a.ResID < b.ResID
	})
}

func compareBySortSpec(tenantID string, a, b SearchResult, s SortSpec, lookup ResourceLookupFunc) int {
	switch s.Param {
	case "_id":
		return strings.Compare(a.ResID, b.ResID)
	case "_lastUpdated":
		if a.LastUpdated.Before(b.LastUpdated) {
			return -1
		}
		if a.LastUpdated.After(b.LastUpdated) {
			return 1
		}
		return 0
	}
	if lookup == nil {
		return 0
	}
	va, okA := lookup(tenantID, a.ResType, a.ResID)
	vb, okB := lookup(tenantID, b.ResType, b.ResID)
	if !okA || !okB {
		return 0
	}
	cat := SearchParamCategory(s.Param)
	switch cat {
	case "date":
		da, hasA := minIndexDate(va.Indexes, s.Param)
		db, hasB := minIndexDate(vb.Indexes, s.Param)
		if !hasA && !hasB {
			return 0
		}
		if !hasA {
			return 1
		}
		if !hasB {
			return -1
		}
		if da.Before(db) {
			return -1
		}
		if da.After(db) {
			return 1
		}
		return 0
	case "quantity":
		qa, hasA := minIndexQuantity(va.Indexes, s.Param)
		qb, hasB := minIndexQuantity(vb.Indexes, s.Param)
		if !hasA && !hasB {
			return 0
		}
		if !hasA {
			return 1
		}
		if !hasB {
			return -1
		}
		if qa < qb {
			return -1
		}
		if qa > qb {
			return 1
		}
		return 0
	default:
		sa := minIndexLexical(va.Indexes, s.Param)
		sb := minIndexLexical(vb.Indexes, s.Param)
		if sa == "" && sb == "" {
			return 0
		}
		if sa == "" {
			return 1
		}
		if sb == "" {
			return -1
		}
		return strings.Compare(sa, sb)
	}
}

func minIndexDate(idx *SearchIndexes, spName string) (time.Time, bool) {
	if idx == nil {
		return time.Time{}, false
	}
	var best time.Time
	found := false
	for _, d := range idx.Dates {
		if d.SpName == spName {
			if !found || d.SpLow.Before(best) {
				best = d.SpLow
				found = true
			}
		}
	}
	return best, found
}

func minIndexQuantity(idx *SearchIndexes, spName string) (float64, bool) {
	if idx == nil {
		return 0, false
	}
	best := 0.0
	found := false
	for _, q := range idx.Quantities {
		if q.SpName == spName {
			if !found || q.SpValue < best {
				best = q.SpValue
				found = true
			}
		}
	}
	return best, found
}

func minIndexLexical(idx *SearchIndexes, spName string) string {
	if idx == nil {
		return ""
	}
	best := ""
	consider := func(v string) {
		low := strings.ToLower(strings.TrimSpace(v))
		if low != "" && (best == "" || low < best) {
			best = low
		}
	}
	for _, s := range idx.Strings {
		if s.SpName == spName {
			consider(s.SpValue)
		}
	}
	for _, t := range idx.Tokens {
		if t.SpName == spName {
			consider(t.SpValue)
		}
	}
	for _, r := range idx.References {
		if r.SpName == spName {
			consider(r.TargetType + "/" + r.TargetID)
		}
	}
	for _, u := range idx.URIs {
		if u.SpName == spName {
			consider(u.SpValue)
		}
	}
	return best
}

// ResolveSearchIncludes resolves forward _include and reverse _revinclude (including :iterate)
// starting from the primary search matches.
func ResolveSearchIncludes(
	tenantID string,
	matches []SearchResult,
	includes, revIncludes []IncludeParam,
	lookup ResourceLookupFunc,
	listTenant TenantResourcesFunc,
) []SearchResult {
	if len(matches) == 0 || (len(includes) == 0 && len(revIncludes) == 0) {
		return nil
	}
	seen := make(map[string]bool, len(matches))
	for _, m := range matches {
		seen[tenantID+"|"+m.ResType+"|"+m.ResID] = true
	}

	var out []SearchResult
	appendWave := func(frontier []SearchResult, onlyIterate bool) []SearchResult {
		var added []SearchResult
		for _, inc := range includes {
			if onlyIterate && !inc.Iterate {
				continue
			}
			for _, m := range frontier {
				if inc.SourceType != "" && inc.SourceType != "*" && !strings.EqualFold(inc.SourceType, m.ResType) {
					continue
				}
				if lookup == nil {
					continue
				}
				mv, ok := lookup(tenantID, m.ResType, m.ResID)
				if !ok || mv.Indexes == nil {
					continue
				}
				for _, ref := range mv.Indexes.References {
					if inc.ParamName != "" && inc.ParamName != "*" && !strings.EqualFold(inc.ParamName, ref.SpName) {
						continue
					}
					if inc.TargetType != "" && inc.TargetType != "*" && ref.TargetType != "" && !strings.EqualFold(inc.TargetType, ref.TargetType) {
						continue
					}
					for _, target := range resolveReferencedTargets(tenantID, inc.TargetType, ref.TargetType, ref.TargetID, lookup, listTenant) {
						key := tenantID + "|" + target.ResType + "|" + target.ResID
						if !seen[key] {
							seen[key] = true
							sr := SearchResult{
								ResType:       target.ResType,
								ResID:         target.ResID,
								ResVersion:    target.ResVersion,
								LastUpdated:   target.LastUpdated,
								ResourceProto: append([]byte(nil), target.ResourceProto...),
							}
							out = append(out, sr)
							added = append(added, sr)
						}
					}
				}
			}
		}
		if listTenant != nil {
			allTenant := listTenant(tenantID)
			for _, rev := range revIncludes {
				if onlyIterate && !rev.Iterate {
					continue
				}
				for _, cand := range allTenant {
					if cand.Indexes == nil {
						continue
					}
					if rev.SourceType != "" && rev.SourceType != "*" && !strings.EqualFold(rev.SourceType, cand.ResType) {
						continue
					}
					key := tenantID + "|" + cand.ResType + "|" + cand.ResID
					if seen[key] {
						continue
					}
					for _, ref := range cand.Indexes.References {
						if rev.ParamName != "" && rev.ParamName != "*" && !strings.EqualFold(rev.ParamName, ref.SpName) {
							continue
						}
						matchedTarget := false
						for _, m := range frontier {
							if rev.TargetType != "" && rev.TargetType != "*" && !strings.EqualFold(rev.TargetType, m.ResType) {
								continue
							}
							if ref.TargetID == m.ResID && (ref.TargetType == "" || strings.EqualFold(ref.TargetType, m.ResType)) {
								matchedTarget = true
								break
							}
						}
						if matchedTarget {
							seen[key] = true
							sr := SearchResult{
								ResType:       cand.ResType,
								ResID:         cand.ResID,
								ResVersion:    cand.ResVersion,
								LastUpdated:   cand.LastUpdated,
								ResourceProto: append([]byte(nil), cand.ResourceProto...),
							}
							out = append(out, sr)
							added = append(added, sr)
							break
						}
					}
				}
			}
		}
		return added
	}

	frontier := appendWave(matches, false)
	for pass := 0; pass < 3 && len(frontier) > 0; pass++ {
		frontier = appendWave(frontier, true)
	}
	return out
}
