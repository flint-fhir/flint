package server

import (
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type quantityInfo struct {
	val     float64
	decText string
	unit    string
	code    string
	isCal   bool
	hasUnit bool
}

var calendarUnitToUCUM = map[string]string{
	"year":         "a",
	"years":        "a",
	"month":        "mo",
	"months":       "mo",
	"week":         "wk",
	"weeks":        "wk",
	"day":          "d",
	"days":         "d",
	"hour":         "h",
	"hours":        "h",
	"minute":       "min",
	"minutes":      "min",
	"second":       "s",
	"seconds":      "s",
	"millisecond":  "ms",
	"milliseconds": "ms",
}

type ucumUnitDef struct {
	dim    string
	factor float64
}

var ucumCanonicalTable = map[string]ucumUnitDef{
	"1":       {dim: "dimless", factor: 1},
	"g":       {dim: "mass", factor: 1},
	"kg":      {dim: "mass", factor: 1000},
	"mg":      {dim: "mass", factor: 0.001},
	"ug":      {dim: "mass", factor: 1e-6},
	"[lb_av]": {dim: "mass", factor: 453.59237},
	"lbs":     {dim: "mass", factor: 453.59237},
	"[oz_av]": {dim: "mass", factor: 28.349523125},
	"m":       {dim: "length", factor: 1},
	"cm":      {dim: "length", factor: 0.01},
	"mm":      {dim: "length", factor: 0.001},
	"km":      {dim: "length", factor: 1000},
	"[in_i]":  {dim: "length", factor: 0.0254},
	"[ft_i]":  {dim: "length", factor: 0.3048},
	"m2":      {dim: "area", factor: 1},
	"cm2":     {dim: "area", factor: 0.0001},
	"L":       {dim: "volume", factor: 1},
	"l":       {dim: "volume", factor: 1},
	"mL":      {dim: "volume", factor: 0.001},
	"ml":      {dim: "volume", factor: 0.001},
	"s":       {dim: "time", factor: 1},
	"[s]":     {dim: "time", factor: 1},
	"ms":      {dim: "time", factor: 0.001},
	"min":     {dim: "time", factor: 60},
	"h":       {dim: "time", factor: 3600},
	"d":       {dim: "time", factor: 86400},
	"wk":      {dim: "time", factor: 604800},
	"mo":      {dim: "time", factor: 2629800},
	"a":       {dim: "time", factor: 31557600},
	"g/m":     {dim: "g/m", factor: 1},
}

var reQuantityUCUM = regexp.MustCompile(`^([+-]?\d+(?:\.\d+)?)\s+'([^']+)'$`)
var reQuantityCal = regexp.MustCompile(`^([+-]?\d+(?:\.\d+)?)\s+([a-zA-Z]+)$`)

func parseQuantityLiteral(seg string) (fhirTypedValue, bool) {
	seg = strings.TrimSpace(seg)
	if m := reQuantityUCUM.FindStringSubmatch(seg); m != nil {
		fv, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return fhirTypedValue{}, false
		}
		u := m[2]
		return fhirTypedValue{
			val: map[string]any{
				"value":    fv,
				"unit":     u,
				"code":     u,
				"system":   "http://unitsofmeasure.org",
				"_calUnit": false,
				"_decText": m[1],
			},
			fhirType: "System.Quantity",
			decText:  m[1] + " '" + u + "'",
		}, true
	}
	if m := reQuantityCal.FindStringSubmatch(seg); m != nil {
		calWord := strings.ToLower(m[2])
		ucum, ok := calendarUnitToUCUM[calWord]
		if !ok {
			return fhirTypedValue{}, false
		}
		fv, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return fhirTypedValue{}, false
		}
		return fhirTypedValue{
			val: map[string]any{
				"value":    fv,
				"unit":     m[2],
				"code":     ucum,
				"system":   "http://unitsofmeasure.org",
				"_calUnit": true,
				"_decText": m[1],
			},
			fhirType: "System.Quantity",
			decText:  m[1] + " " + m[2],
		}, true
	}
	return fhirTypedValue{}, false
}

func extractQuantityInfo(v any) (quantityInfo, bool) {
	u := unwrapFHIRTyped(v)
	m, ok := u.(map[string]any)
	if !ok {
		return quantityInfo{}, false
	}
	rawVal, hasVal := m["value"]
	if !hasVal {
		return quantityInfo{}, false
	}
	if tv, isTV := v.(fhirTypedValue); isTV {
		tName := strings.TrimPrefix(tv.fhirType, "System.")
		if tName != "" && !isFHIRSubtypeOf(tName, "Quantity") {
			return quantityInfo{}, false
		}
	}
	fv, okF := toFloat(rawVal)
	if !okF {
		if s, okS := rawVal.(string); okS {
			parsed, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return quantityInfo{}, false
			}
			fv = parsed
		} else {
			return quantityInfo{}, false
		}
	}
	unit, _ := m["unit"].(string)
	code, _ := m["code"].(string)
	isCal, _ := m["_calUnit"].(bool)
	dt, _ := m["_decText"].(string)
	if dt == "" {
		if pm, okPM := m["_value"].(map[string]any); okPM {
			dt, _ = pm["_decimalText"].(string)
		}
	}
	hasUnit := unit != "" || code != ""
	return quantityInfo{
		val:     fv,
		decText: dt,
		unit:    unit,
		code:    code,
		isCal:   isCal,
		hasUnit: hasUnit,
	}, true
}

func formatQuantityString(v any) (string, bool) {
	q, ok := extractQuantityInfo(v)
	if !ok {
		return "", false
	}
	numStr := q.decText
	if numStr == "" {
		numStr = strconv.FormatFloat(q.val, 'f', -1, 64)
	}
	if q.isCal {
		return numStr + " " + q.unit, true
	}
	u := q.code
	if u == "" {
		u = q.unit
	}
	if u == "" {
		return numStr, true
	}
	return numStr + " '" + u + "'", true
}

func toQuantityConv(v any, targetUnit string) (fhirTypedValue, bool) {
	if q, ok := extractQuantityInfo(v); ok {
		if targetUnit == "" || q.code == targetUnit || q.unit == targetUnit {
			if tv, isTV := v.(fhirTypedValue); isTV {
				return tv, true
			}
			return fhirTypedValue{val: unwrapFHIRTyped(v), fhirType: "Quantity"}, true
		}
		return fhirTypedValue{}, false
	}
	if tv, isTV := v.(fhirTypedValue); isTV && isTemporalFHIRType(tv.fhirType) {
		return fhirTypedValue{}, false
	}
	u := unwrapFHIRTyped(v)
	switch val := u.(type) {
	case int:
		s := strconv.Itoa(val)
		return parseQuantityLiteral(s + " '1'")
	case int64:
		s := strconv.FormatInt(val, 10)
		return parseQuantityLiteral(s + " '1'")
	case float64:
		dt := strconv.FormatFloat(val, 'f', -1, 64)
		if tv, isTV := v.(fhirTypedValue); isTV && tv.decText != "" {
			dt = tv.decText
		}
		return parseQuantityLiteral(dt + " '1'")
	case bool:
		if val {
			return parseQuantityLiteral("1.0 '1'")
		}
		return parseQuantityLiteral("0.0 '1'")
	case string:
		s := strings.TrimSpace(val)
		if qv, okQ := parseQuantityLiteral(s); okQ {
			return qv, true
		}
		if dtv, okD := toDecimalTypedConv(s); okD {
			return parseQuantityLiteral(dtv.decText + " '1'")
		}
	}
	return fhirTypedValue{}, false
}

func effectiveQuantityCode(q quantityInfo) string {
	if q.code != "" {
		return q.code
	}
	return q.unit
}

func isAboveDayCalendarQuantity(q quantityInfo) bool {
	return q.isCal && (q.code == "a" || q.code == "mo")
}

func quantitiesComparable(a, b any) (bool, bool) {
	qa, okA := extractQuantityInfo(a)
	qb, okB := extractQuantityInfo(b)
	if !okA || !okB {
		return false, false
	}
	ca := effectiveQuantityCode(qa)
	cb := effectiveQuantityCode(qb)
	if ca == cb {
		return true, true
	}
	da, okDA := ucumCanonicalTable[ca]
	db, okDB := ucumCanonicalTable[cb]
	if okDA && okDB {
		return da.dim == db.dim, true
	}
	return false, true
}

func compareQuantitiesTri(qa, qb quantityInfo, forEquivalence bool) (int, bool) {
	if !qa.hasUnit && !qb.hasUnit {
		return compareFloatWithScale(qa.val, qa.decText, qb.val, qb.decText, forEquivalence), true
	}
	if qa.hasUnit != qb.hasUnit {
		return 0, false
	}
	ca := effectiveQuantityCode(qa)
	cb := effectiveQuantityCode(qb)

	if !forEquivalence && (isAboveDayCalendarQuantity(qa) || isAboveDayCalendarQuantity(qb)) {
		if qa.isCal && qb.isCal && ca == cb {
			return compareFloatWithScale(qa.val, qa.decText, qb.val, qb.decText, false), true
		}
		return 0, false
	}

	if ca == cb {
		return compareFloatWithScale(qa.val, qa.decText, qb.val, qb.decText, forEquivalence), true
	}

	da, okDA := ucumCanonicalTable[ca]
	db, okDB := ucumCanonicalTable[cb]
	if !okDA || !okDB || da.dim != db.dim {
		return 0, false
	}
	va := qa.val * da.factor
	vb := qb.val * db.factor
	if forEquivalence {
		minMag := math.Min(da.factor, db.factor)
		tol := 0.5 * math.Max(da.factor, db.factor)
		if qa.decText != "" && strings.Contains(qa.decText, ".") {
			tol = math.Min(tol, 0.5*math.Pow(10, -float64(fracDigitsCount(qa.decText)))*da.factor)
		}
		if minMag > 0 && math.Abs(va-vb) <= tol+1e-9 {
			return 0, true
		}
	}
	if math.Abs(va-vb) <= 1e-9 {
		return 0, true
	}
	if va < vb {
		return -1, true
	}
	return 1, true
}

func multiplyQuantities(qa, qb quantityInfo) (fhirTypedValue, bool) {
	ca := effectiveQuantityCode(qa)
	cb := effectiveQuantityCode(qb)
	da, okDA := ucumCanonicalTable[ca]
	db, okDB := ucumCanonicalTable[cb]
	if okDA && okDB && da.dim == "length" && db.dim == "length" {
		m2Val := (qa.val * da.factor) * (qb.val * db.factor)
		dt := strconv.FormatFloat(m2Val, 'f', -1, 64)
		return parseQuantityLiteral(dt + " 'm2'")
	}
	u := ca + "." + cb
	if ca == "1" {
		u = cb
	} else if cb == "1" {
		u = ca
	}
	prod := qa.val * qb.val
	return parseQuantityLiteral(strconv.FormatFloat(prod, 'f', -1, 64) + " '" + u + "'")
}

func divideQuantities(qa, qb quantityInfo) (fhirTypedValue, bool) {
	if qb.val == 0 {
		return fhirTypedValue{}, false
	}
	ca := effectiveQuantityCode(qa)
	cb := effectiveQuantityCode(qb)
	if ca == cb {
		quot := qa.val / qb.val
		return parseQuantityLiteral(strconv.FormatFloat(quot, 'f', -1, 64) + " '1'")
	}
	da, okDA := ucumCanonicalTable[ca]
	db, okDB := ucumCanonicalTable[cb]
	if okDA && okDB && da.dim == db.dim {
		quot := (qa.val * da.factor) / (qb.val * db.factor)
		return parseQuantityLiteral(strconv.FormatFloat(quot, 'f', -1, 64) + " '1'")
	}
	quot := qa.val / qb.val
	u := ca + "/" + cb
	return parseQuantityLiteral(strconv.FormatFloat(quot, 'f', -1, 64) + " '" + u + "'")
}

// Temporal Parsing & Comparison

func isTemporalFHIRType(ft string) bool {
	switch ft {
	case "System.Date", "System.DateTime", "System.Time", "date", "dateTime", "instant", "time":
		return true
	}
	return false
}

var (
	reDateOnly     = regexp.MustCompile(`^(\d{4})(?:-(\d{2})(?:-(\d{2}))?)?$`)
	reDateTimeFull = regexp.MustCompile(`^(\d{4})(?:-(\d{2})(?:-(\d{2})(?:T(?:(\d{2})(?::(\d{2})(?::(\d{2})(?:\.(\d+))?)?)?)?(Z|[+-]\d{2}:\d{2})?)?)?)?$`)
	reTimeOnly     = regexp.MustCompile(`^T?(\d{2})(?::(\d{2})(?::(\d{2})(?:\.(\d+))?)?)?$`)
)

func toDateConv(v any) (fhirTypedValue, bool) {
	if tv, isTV := v.(fhirTypedValue); isTV {
		switch tv.fhirType {
		case "System.Date", "date":
			return tv, true
		case "System.DateTime", "dateTime", "instant":
			if s, ok := tv.val.(string); ok {
				datePart := s
				if idx := strings.IndexByte(datePart, 'T'); idx != -1 {
					datePart = datePart[:idx]
				}
				if reDateOnly.MatchString(datePart) {
					return fhirTypedValue{val: datePart, fhirType: "System.Date", decText: datePart}, true
				}
			}
			return fhirTypedValue{}, false
		case "System.Time", "time":
			return fhirTypedValue{}, false
		}
		v = tv.val
	}
	s, ok := v.(string)
	if !ok {
		return fhirTypedValue{}, false
	}
	s = strings.TrimPrefix(strings.TrimSpace(s), "@")
	if !reDateOnly.MatchString(s) {
		return fhirTypedValue{}, false
	}
	return fhirTypedValue{val: s, fhirType: "System.Date", decText: s}, true
}

func toDateTimeConv(v any) (fhirTypedValue, bool) {
	if tv, isTV := v.(fhirTypedValue); isTV {
		switch tv.fhirType {
		case "System.DateTime", "dateTime", "instant":
			return tv, true
		case "System.Date", "date":
			if s, ok := tv.val.(string); ok && reDateOnly.MatchString(s) {
				return fhirTypedValue{val: s, fhirType: "System.DateTime", decText: s}, true
			}
			return fhirTypedValue{}, false
		case "System.Time", "time":
			return fhirTypedValue{}, false
		}
		v = tv.val
	}
	s, ok := v.(string)
	if !ok {
		return fhirTypedValue{}, false
	}
	s = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s), "@"), "T")
	if s == "" || !reDateTimeFull.MatchString(s) {
		return fhirTypedValue{}, false
	}
	return fhirTypedValue{val: s, fhirType: "System.DateTime", decText: s}, true
}

func toTimeConv(v any) (fhirTypedValue, bool) {
	if tv, isTV := v.(fhirTypedValue); isTV {
		switch tv.fhirType {
		case "System.Time", "time":
			return tv, true
		case "System.Date", "date", "System.DateTime", "dateTime", "instant":
			return fhirTypedValue{}, false
		}
		v = tv.val
	}
	s, ok := v.(string)
	if !ok {
		return fhirTypedValue{}, false
	}
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "@")
	if strings.ContainsAny(s, "Z+-") {
		return fhirTypedValue{}, false
	}
	m := reTimeOnly.FindStringSubmatch(s)
	if m == nil {
		return fhirTypedValue{}, false
	}
	clean := strings.TrimPrefix(s, "T")
	return fhirTypedValue{val: clean, fhirType: "System.Time", decText: "T" + clean}, true
}

type temporalParts struct {
	isTime  bool
	prec    int // 1=Y, 2=M, 3=D, 4=H, 5=Min, 6=Sec/Ms (for Date/DateTime); for Time: 4=H, 5=Min, 6=Sec/Ms
	year    int
	month   int
	day     int
	hour    int
	minute  int
	second  int
	nanos   int
	hasTZ   bool
	tzOffS  int
	rawText string
}

func extractTemporalParts(v any) (temporalParts, bool) {
	var ft string
	var s string
	if tv, isTV := v.(fhirTypedValue); isTV {
		ft = tv.fhirType
		str, ok := tv.val.(string)
		if !ok {
			return temporalParts{}, false
		}
		s = str
	} else {
		return temporalParts{}, false
	}
	if !isTemporalFHIRType(ft) {
		return temporalParts{}, false
	}
	s = strings.TrimSpace(s)
	if ft == "System.Time" || ft == "time" {
		m := reTimeOnly.FindStringSubmatch(s)
		if m == nil {
			return temporalParts{}, false
		}
		tp := temporalParts{isTime: true, prec: 4, rawText: strings.TrimPrefix(s, "T")}
		tp.hour, _ = strconv.Atoi(m[1])
		if m[2] != "" {
			tp.prec = 5
			tp.minute, _ = strconv.Atoi(m[2])
		}
		if m[3] != "" {
			tp.prec = 6
			tp.second, _ = strconv.Atoi(m[3])
		}
		if m[4] != "" {
			tp.nanos = parseFracNanos(m[4])
		}
		return tp, true
	}

	norm := strings.TrimSuffix(s, "T")
	m := reDateTimeFull.FindStringSubmatch(norm)
	if m == nil {
		return temporalParts{}, false
	}
	tp := temporalParts{prec: 1, month: 1, day: 1, rawText: s}
	tp.year, _ = strconv.Atoi(m[1])
	if m[2] != "" {
		tp.prec = 2
		tp.month, _ = strconv.Atoi(m[2])
	}
	if m[3] != "" {
		tp.prec = 3
		tp.day, _ = strconv.Atoi(m[3])
	}
	if m[4] != "" {
		tp.prec = 4
		tp.hour, _ = strconv.Atoi(m[4])
	}
	if m[5] != "" {
		tp.prec = 5
		tp.minute, _ = strconv.Atoi(m[5])
	}
	if m[6] != "" {
		tp.prec = 6
		tp.second, _ = strconv.Atoi(m[6])
	}
	if m[7] != "" {
		tp.nanos = parseFracNanos(m[7])
	}
	if m[8] != "" {
		tp.hasTZ = true
		tp.tzOffS = parseTZOffsetSeconds(m[8])
	}
	return tp, true
}

func parseFracNanos(frac string) int {
	if len(frac) > 9 {
		frac = frac[:9]
	} else {
		frac += strings.Repeat("0", 9-len(frac))
	}
	n, _ := strconv.Atoi(frac)
	return n
}

func parseTZOffsetSeconds(tz string) int {
	if tz == "Z" || tz == "z" {
		return 0
	}
	if len(tz) == 6 && (tz[0] == '+' || tz[0] == '-') {
		hh, _ := strconv.Atoi(tz[1:3])
		mm, _ := strconv.Atoi(tz[4:6])
		sec := hh*3600 + mm*60
		if tz[0] == '-' {
			sec = -sec
		}
		return sec
	}
	return 0
}

func compareTemporalsTri(a, b temporalParts) (int, bool, bool) {
	// returns (cmp, ok, diffKind): diffKind=true when Date/DateTime vs Time (never equal, not uncertain)
	if a.isTime != b.isTime {
		return 0, false, true
	}
	if a.isTime {
		valsA := [4]int{a.hour, a.minute, a.second, a.nanos}
		valsB := [4]int{b.hour, b.minute, b.second, b.nanos}
		minP := a.prec
		if b.prec < minP {
			minP = b.prec
		}
		// prec 4 -> idx 0; prec 5 -> idx 1; prec 6 -> idx 2 + nanos idx 3
		maxIdx := minP - 4
		for i := 0; i <= maxIdx; i++ {
			if valsA[i] < valsB[i] {
				return -1, true, false
			}
			if valsA[i] > valsB[i] {
				return 1, true, false
			}
		}
		if a.prec != b.prec {
			return 0, false, false
		}
		if a.prec == 6 {
			if a.nanos < b.nanos {
				return -1, true, false
			}
			if a.nanos > b.nanos {
				return 1, true, false
			}
		}
		return 0, true, false
	}

	// Both have TZ and equal precision (>= 4)
	if a.hasTZ && b.hasTZ && a.prec == b.prec {
		ta := time.Date(a.year, time.Month(a.month), a.day, a.hour, a.minute, a.second, a.nanos, time.FixedZone("", a.tzOffS))
		tb := time.Date(b.year, time.Month(b.month), b.day, b.hour, b.minute, b.second, b.nanos, time.FixedZone("", b.tzOffS))
		if ta.Before(tb) {
			return -1, true, false
		}
		if ta.After(tb) {
			return 1, true, false
		}
		return 0, true, false
	}

	// One has TZ and the other does not (or mixed date vs dateTime with TZ): check worst-case [-14h, +14h]
	if a.hasTZ != b.hasTZ {
		minA, maxA := temporalInstantBounds(a)
		minB, maxB := temporalInstantBounds(b)
		if maxA.Before(minB) {
			return -1, true, false
		}
		if minA.After(maxB) {
			return 1, true, false
		}
		return 0, false, false
	}

	// Neither has TZ (or both have TZ with different precisions)
	if a.hasTZ && b.hasTZ && a.prec != b.prec {
		minA, maxA := temporalInstantBounds(a)
		minB, maxB := temporalInstantBounds(b)
		if maxA.Before(minB) {
			return -1, true, false
		}
		if minA.After(maxB) {
			return 1, true, false
		}
		return 0, false, false
	}

	valsA := [6]int{a.year, a.month, a.day, a.hour, a.minute, a.second}
	valsB := [6]int{b.year, b.month, b.day, b.hour, b.minute, b.second}
	minP := a.prec
	if b.prec < minP {
		minP = b.prec
	}
	for i := 0; i < minP; i++ {
		if valsA[i] < valsB[i] {
			return -1, true, false
		}
		if valsA[i] > valsB[i] {
			return 1, true, false
		}
	}
	if a.prec != b.prec {
		return 0, false, false
	}
	if a.prec == 6 {
		if a.nanos < b.nanos {
			return -1, true, false
		}
		if a.nanos > b.nanos {
			return 1, true, false
		}
	}
	return 0, true, false
}

func temporalInstantBounds(tp temporalParts) (time.Time, time.Time) {
	y1, m1, d1, h1, min1, s1, ns1 := tp.year, 1, 1, 0, 0, 0, 0
	y2, m2, d2, h2, min2, s2, ns2 := tp.year, 12, 31, 23, 59, 59, 999999999
	if tp.prec >= 2 {
		m1, m2 = tp.month, tp.month
		d2 = daysInMonth(tp.year, tp.month)
	}
	if tp.prec >= 3 {
		d1, d2 = tp.day, tp.day
	}
	if tp.prec >= 4 {
		h1, h2 = tp.hour, tp.hour
	}
	if tp.prec >= 5 {
		min1, min2 = tp.minute, tp.minute
	}
	if tp.prec >= 6 {
		s1, s2 = tp.second, tp.second
		ns1, ns2 = tp.nanos, tp.nanos
	}
	if tp.hasTZ {
		loc := time.FixedZone("", tp.tzOffS)
		return time.Date(y1, time.Month(m1), d1, h1, min1, s1, ns1, loc),
			time.Date(y2, time.Month(m2), d2, h2, min2, s2, ns2, loc)
	}
	// Unknown TZ: earliest instant is +14:00, latest instant is -14:00
	locEarly := time.FixedZone("", 14*3600)
	locLate := time.FixedZone("", -14*3600)
	return time.Date(y1, time.Month(m1), d1, h1, min1, s1, ns1, locEarly),
		time.Date(y2, time.Month(m2), d2, h2, min2, s2, ns2, locLate)
}

func daysInMonth(year, month int) int {
	return time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func evalTemporalDurationOp(lhs any, op string, rhs any) ([]any, bool, bool) {
	tp, okT := extractTemporalParts(lhs)
	if !okT {
		return nil, false, false
	}
	q, okQ := extractQuantityInfo(rhs)
	if !okQ {
		return nil, true, true
	}
	tvL, _ := lhs.(fhirTypedValue)
	sign := 1
	if op == "-" {
		sign = -1
	}

	rawUnit := q.unit
	if !q.isCal && q.code != "" {
		rawUnit = q.code
	}
	var years, months, days int
	var dur time.Duration
	whole := int(math.Trunc(q.val)) * sign

	switch rawUnit {
	case "year", "years":
		years = whole
	case "month", "months":
		months = whole
	case "week", "weeks", "wk":
		days = whole * 7
	case "day", "days", "d":
		days = whole
	case "hour", "hours", "h":
		if tp.prec < 4 && !tp.isTime {
			return nil, true, true
		}
		dur = time.Duration(whole) * time.Hour
	case "minute", "minutes", "min":
		if tp.prec < 4 && !tp.isTime {
			return nil, true, true
		}
		dur = time.Duration(whole) * time.Minute
	case "second", "seconds", "s":
		if tp.prec < 4 && !tp.isTime {
			return nil, true, true
		}
		if rawUnit == "s" && !q.isCal && math.Trunc(q.val) != q.val {
			// FHIRPath R4 §6.6.1: fractional 's' truncates to the precision of the operand (or whole seconds if prec <= 6)
			dur = time.Duration(int64(math.Trunc(q.val))) * time.Second * time.Duration(sign)
		} else {
			dur = time.Duration(whole) * time.Second
		}
	case "millisecond", "milliseconds", "ms":
		if tp.prec < 4 && !tp.isTime {
			return nil, true, true
		}
		dur = time.Duration(int64(math.Trunc(q.val))*int64(sign)) * time.Millisecond
	default:
		// UCUM 'a', 'mo', 'cm', etc. are invalid in Date/DateTime +/- arithmetic
		return nil, true, true
	}

	loc := time.UTC
	if tp.hasTZ {
		loc = time.FixedZone("", tp.tzOffS)
	}
	base := time.Date(tp.year, time.Month(tp.month), tp.day, tp.hour, tp.minute, tp.second, tp.nanos, loc)
	res := base.AddDate(years, months, days).Add(dur)
	formatted := formatTemporalWithInputStyle(res, tvL)
	return []any{fhirTypedValue{
		val:      formatted,
		fhirType: tvL.fhirType,
		decText:  formatted,
	}}, true, false
}

func formatTemporalWithInputStyle(t time.Time, orig fhirTypedValue) string {
	raw, _ := orig.val.(string)
	if orig.decText != "" {
		raw = orig.decText
	}
	raw = strings.TrimPrefix(raw, "@")
	if orig.fhirType == "System.Date" || orig.fhirType == "date" {
		switch len(raw) {
		case 4:
			return fmt.Sprintf("%04d", t.Year())
		case 7:
			return fmt.Sprintf("%04d-%02d", t.Year(), int(t.Month()))
		default:
			return fmt.Sprintf("%04d-%02d-%02d", t.Year(), int(t.Month()), t.Day())
		}
	}
	// Preserve original precision & TZ suffix string
	tzSuffix := ""
	core := raw
	if strings.HasSuffix(core, "Z") {
		tzSuffix = "Z"
		core = core[:len(core)-1]
	} else if len(core) >= 6 && (core[len(core)-6] == '+' || core[len(core)-6] == '-') && core[len(core)-3] == ':' {
		tzSuffix = core[len(core)-6:]
		core = core[:len(core)-6]
	}
	fracLen := 0
	if dot := strings.IndexByte(core, '.'); dot != -1 {
		fracLen = len(core) - dot - 1
	}
	tIdx := strings.IndexByte(core, 'T')
	if tIdx == -1 {
		switch len(core) {
		case 4:
			return fmt.Sprintf("%04d", t.Year())
		case 7:
			return fmt.Sprintf("%04d-%02d", t.Year(), int(t.Month()))
		default:
			return fmt.Sprintf("%04d-%02d-%02d", t.Year(), int(t.Month()), t.Day())
		}
	}
	timePart := core[tIdx+1:]
	colons := strings.Count(timePart, ":")
	switch colons {
	case 0:
		return fmt.Sprintf("%04d-%02d-%02dT%02d%s", t.Year(), int(t.Month()), t.Day(), t.Hour(), tzSuffix)
	case 1:
		return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d%s", t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute(), tzSuffix)
	default:
		if fracLen > 0 {
			nsStr := fmt.Sprintf("%09d", t.Nanosecond())
			if fracLen < 9 {
				nsStr = nsStr[:fracLen]
			}
			return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d.%s%s", t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute(), t.Second(), nsStr, tzSuffix)
		}
		return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d%s", t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute(), t.Second(), tzSuffix)
	}
}

// Precision & Boundary Functions

func fracDigitsCount(s string) int {
	s = strings.TrimSpace(s)
	if eIdx := strings.IndexAny(s, "eE"); eIdx != -1 {
		mant := s[:eIdx]
		exp, _ := strconv.Atoi(s[eIdx+1:])
		m := 0
		if dot := strings.IndexByte(mant, '.'); dot != -1 {
			m = len(mant) - dot - 1
		}
		if m-exp > 0 {
			return m - exp
		}
		return 0
	}
	if dot := strings.IndexByte(s, '.'); dot != -1 {
		return len(s) - dot - 1
	}
	return 0
}

func evalPrecision(v any) (int, bool) {
	if tp, okT := extractTemporalParts(v); okT {
		raw := tp.rawText
		if !tp.isTime {
			if strings.HasSuffix(raw, "Z") {
				raw = raw[:len(raw)-1]
			} else if len(raw) >= 6 && (raw[len(raw)-6] == '+' || raw[len(raw)-6] == '-') && raw[len(raw)-3] == ':' {
				raw = raw[:len(raw)-6]
			}
		}
		digits := 0
		for i := 0; i < len(raw); i++ {
			if raw[i] >= '0' && raw[i] <= '9' {
				digits++
			}
		}
		return digits, true
	}
	if tv, isTV := v.(fhirTypedValue); isTV && (tv.fhirType == "System.Decimal" || tv.fhirType == "decimal") {
		if tv.decText != "" {
			return fracDigitsCount(tv.decText), true
		}
	}
	if _, okF := toFloat(v); okF {
		if _, okI := toIntStrict(v); okI {
			return 0, true
		}
		s := fmt.Sprintf("%v", unwrapFHIRTyped(v))
		return fracDigitsCount(s), true
	}
	return 0, false
}

func evalBoundary(v any, isLow bool, hasPrec bool, prec int) (any, bool) {
	if q, okQ := extractQuantityInfo(v); okQ {
		dt := q.decText
		if dt == "" {
			dt = strconv.FormatFloat(q.val, 'f', -1, 64)
		}
		outStr, outFloat, okB := computeNumericBoundary(dt, isLow, hasPrec, prec)
		if !okB {
			return nil, false
		}
		tvOrig, _ := v.(fhirTypedValue)
		origMap, _ := unwrapFHIRTyped(v).(map[string]any)
		cloned := make(map[string]any, len(origMap)+2)
		for k, mv := range origMap {
			cloned[k] = mv
		}
		cloned["value"] = outFloat
		cloned["_decText"] = outStr
		ft := tvOrig.fhirType
		if ft == "" {
			ft = "Quantity"
		}
		resTV := fhirTypedValue{val: cloned, fhirType: ft}
		if qStr, okS := formatQuantityString(resTV); okS {
			resTV.decText = qStr
		}
		return resTV, true
	}

	if tp, okT := extractTemporalParts(v); okT {
		tvOrig, _ := v.(fhirTypedValue)
		return computeTemporalBoundary(tp, tvOrig, isLow, hasPrec, prec)
	}

	var numStr string
	if tv, isTV := v.(fhirTypedValue); isTV {
		if tv.decText != "" {
			numStr = tv.decText
		} else if fv, ok := toFloat(tv.val); ok {
			numStr = strconv.FormatFloat(fv, 'f', -1, 64)
		} else {
			return nil, false
		}
	} else if iv, okI := toIntStrict(v); okI {
		numStr = strconv.Itoa(iv)
	} else if fv, okF := v.(float64); okF {
		numStr = strconv.FormatFloat(fv, 'f', -1, 64)
	} else {
		return nil, false
	}

	outStr, outFloat, okB := computeNumericBoundary(numStr, isLow, hasPrec, prec)
	if !okB {
		return nil, false
	}
	if hasPrec && prec == 0 && !strings.Contains(outStr, ".") {
		if iv, err := strconv.Atoi(outStr); err == nil {
			return fhirTypedValue{val: float64(iv), fhirType: "System.Decimal", decText: outStr}, true
		}
	}
	return fhirTypedValue{val: outFloat, fhirType: "System.Decimal", decText: outStr}, true
}

func computeNumericBoundary(raw string, isLow bool, hasPrec bool, prec int) (string, float64, bool) {
	if !hasPrec {
		prec = 8
	}
	if prec < 0 || prec > 31 {
		return "", 0, false
	}
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", 0, false
	}
	isNeg := false
	switch s[0] {
	case '-':
		isNeg = true
		s = s[1:]
	case '+':
		s = s[1:]
	}

	exp := 0
	if eIdx := strings.IndexAny(s, "eE"); eIdx != -1 {
		eVal, err := strconv.Atoi(s[eIdx+1:])
		if err != nil {
			return "", 0, false
		}
		exp = eVal
		s = s[:eIdx]
	}

	fracDigits := 0
	digitsOnly := s
	if dot := strings.IndexByte(s, '.'); dot != -1 {
		fracDigits = len(s) - dot - 1
		digitsOnly = s[:dot] + s[dot+1:]
	}
	// effectiveScale S: value = digitsInt * 10^(-S), halfULP = 5 * 10^(-(S+1))
	S := fracDigits - exp
	digitsInt, ok := new(big.Int).SetString(digitsOnly, 10)
	if !ok {
		return "", 0, false
	}
	ten := big.NewInt(10)

	// Express |x| at scale (S + 1): magScaled = digitsInt * 10
	magScaled := new(big.Int).Mul(digitsInt, ten)
	five := big.NewInt(5)

	// Determine whether boundary moves magnitude UP (+5, round half-up) or DOWN (-5, truncate)
	// lowBoundary: if x >= 0 -> mag - 5 (truncate); if x < 0 -> mag + 5 (round half-up)
	// highBoundary: if x >= 0 -> mag + 5 (round half-up); if x < 0 -> mag - 5 (truncate)
	moveMagUp := (isLow && isNeg) || (!isLow && !isNeg)
	var exactScaled *big.Int // represents magnitude * 10^(S+1)
	if moveMagUp {
		exactScaled = new(big.Int).Add(magScaled, five)
	} else {
		exactScaled = new(big.Int).Sub(magScaled, five)
		if exactScaled.Sign() < 0 {
			// Crossed zero (e.g. 0.0 - 0.05 = -0.05)
			exactScaled.Neg(exactScaled)
			isNeg = !isNeg
			moveMagUp = true
		}
	}

	exactScale := S + 1 // exact value = exactScaled * 10^(-exactScale)
	var targetScaled *big.Int
	if prec >= exactScale {
		shift := prec - exactScale
		pow := new(big.Int).Exp(ten, big.NewInt(int64(shift)), nil)
		targetScaled = new(big.Int).Mul(exactScaled, pow)
	} else {
		drop := exactScale - prec
		div := new(big.Int).Exp(ten, big.NewInt(int64(drop)), nil)
		q, r := new(big.Int).QuoRem(exactScaled, div, new(big.Int))
		if moveMagUp {
			halfDiv := new(big.Int).Quo(div, big.NewInt(2))
			if r.Cmp(halfDiv) >= 0 {
				q.Add(q, big.NewInt(1))
			}
		}
		targetScaled = q
	}

	strMag := formatBigIntWithScale(targetScaled, prec)
	outStr := strMag
	if isNeg && (targetScaled.Sign() > 0 || isLow) {
		outStr = "-" + strMag
	}
	fv, _ := strconv.ParseFloat(outStr, 64)
	return outStr, fv, true
}

func formatBigIntWithScale(v *big.Int, scale int) string {
	s := v.Text(10)
	if scale <= 0 {
		return s
	}
	if len(s) <= scale {
		s = strings.Repeat("0", scale-len(s)+1) + s
	}
	cut := len(s) - scale
	return s[:cut] + "." + s[cut:]
}

func computeTemporalBoundary(tp temporalParts, orig fhirTypedValue, isLow bool, hasPrec bool, prec int) (any, bool) {
	if tp.isTime {
		if !hasPrec {
			prec = 9
		}
		if prec != 2 && prec != 4 && prec != 6 && prec != 9 {
			if prec < 2 || prec > 9 {
				return nil, false
			}
		}
		h, m, s, ms := tp.hour, tp.minute, tp.second, tp.nanos/1_000_000
		if tp.prec < 5 {
			if isLow {
				m = 0
			} else {
				m = 59
			}
		}
		if tp.prec < 6 {
			if isLow {
				s = 0
			} else {
				s = 59
			}
		}
		if !strings.Contains(tp.rawText, ".") {
			if isLow {
				ms = 0
			} else {
				ms = 999
			}
		}
		var out string
		switch {
		case prec <= 2:
			out = fmt.Sprintf("%02d", h)
		case prec <= 4:
			out = fmt.Sprintf("%02d:%02d", h, m)
		case prec <= 6:
			out = fmt.Sprintf("%02d:%02d:%02d", h, m, s)
		default:
			out = fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms)
		}
		return fhirTypedValue{val: out, fhirType: orig.fhirType, decText: "T" + out}, true
	}

	isDateOnly := orig.fhirType == "System.Date" || orig.fhirType == "date"
	if !hasPrec {
		if isDateOnly {
			prec = 8
		} else {
			prec = 17
		}
	}
	if prec < 4 || prec > 17 {
		return nil, false
	}

	y, mo, d, h, mi, s, ms := tp.year, tp.month, tp.day, tp.hour, tp.minute, tp.second, tp.nanos/1_000_000
	if tp.prec < 2 {
		if isLow {
			mo = 1
		} else {
			mo = 12
		}
	}
	if tp.prec < 3 {
		if isLow {
			d = 1
		} else {
			d = daysInMonth(y, mo)
		}
	}
	if tp.prec < 4 {
		if isLow {
			h = 0
		} else {
			h = 23
		}
	}
	// Note HL7 test suite rule (HighBoundaryDateTimeMillisecond1): T08 is normalized to T08:00 internally first!
	if tp.prec == 4 {
		mi = 0
	} else if tp.prec < 5 {
		if isLow {
			mi = 0
		} else {
			mi = 59
		}
	}
	if tp.prec < 6 {
		if isLow {
			s = 0
		} else {
			s = 59
		}
	}
	if !strings.Contains(tp.rawText, ".") {
		if isLow {
			ms = 0
		} else {
			ms = 999
		}
	}

	tzStr := ""
	if tp.hasTZ {
		raw := tp.rawText
		if strings.HasSuffix(raw, "Z") {
			tzStr = "Z"
		} else if len(raw) >= 6 {
			tzStr = raw[len(raw)-6:]
		}
	} else if prec >= 10 {
		if isLow {
			tzStr = "+14:00"
		} else {
			tzStr = "-12:00"
		}
	}

	var out string
	outType := orig.fhirType
	switch {
	case prec <= 4:
		out = fmt.Sprintf("%04d", y)
	case prec <= 6:
		out = fmt.Sprintf("%04d-%02d", y, mo)
	case prec <= 8:
		out = fmt.Sprintf("%04d-%02d-%02d", y, mo, d)
	case prec <= 10:
		out = fmt.Sprintf("%04d-%02d-%02dT%02d%s", y, mo, d, h, tzStr)
		if isDateOnly {
			outType = "System.DateTime"
		}
	case prec <= 12:
		out = fmt.Sprintf("%04d-%02d-%02dT%02d:%02d%s", y, mo, d, h, mi, tzStr)
		if isDateOnly {
			outType = "System.DateTime"
		}
	case prec <= 14:
		out = fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d%s", y, mo, d, h, mi, s, tzStr)
		if isDateOnly {
			outType = "System.DateTime"
		}
	default:
		out = fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d.%03d%s", y, mo, d, h, mi, s, ms, tzStr)
		if isDateOnly {
			outType = "System.DateTime"
		}
	}
	return fhirTypedValue{val: out, fhirType: outType, decText: out}, true
}

// Equality, Equivalence, Ordering & Arithmetic

func compareFloatWithScale(af float64, aDT string, bf float64, bDT string, forEquivalence bool) int {
	if forEquivalence {
		hasScaleA := aDT != "" && strings.Contains(aDT, ".")
		hasScaleB := bDT != "" && strings.Contains(bDT, ".")
		if hasScaleA || hasScaleB {
			minScale := 32
			if hasScaleA {
				sa := fracDigitsCount(aDT)
				if sa < minScale {
					minScale = sa
				}
			}
			if hasScaleB {
				sb := fracDigitsCount(bDT)
				if sb < minScale {
					minScale = sb
				}
			}
			if minScale < 32 {
				pow := math.Pow(10, float64(minScale))
				ra := math.Round(af*pow) / pow
				rb := math.Round(bf*pow) / pow
				if math.Abs(ra-rb) <= 1e-9 {
					return 0
				}
				if ra < rb {
					return -1
				}
				return 1
			}
		}
	}
	if math.Abs(af-bf) <= 1e-9 {
		return 0
	}
	if af < bf {
		return -1
	}
	return 1
}

func fhirPathEqualTri(a, b any) (bool, bool) {
	qa, okQA := extractQuantityInfo(a)
	qb, okQB := extractQuantityInfo(b)
	if okQA || okQB {
		if !okQA || !okQB {
			return false, true
		}
		cmp, ok := compareQuantitiesTri(qa, qb, false)
		if !ok {
			return false, false
		}
		return cmp == 0, true
	}

	ta, okTA := extractTemporalParts(a)
	tb, okTB := extractTemporalParts(b)
	if okTA || okTB {
		if !okTA || !okTB {
			return false, true
		}
		cmp, ok, diffKind := compareTemporalsTri(ta, tb)
		if diffKind {
			return false, true
		}
		if !ok {
			return false, false
		}
		return cmp == 0, true
	}

	af, okFA := toFloat(a)
	bf, okFB := toFloat(b)
	if okFA || okFB {
		if !okFA || !okFB {
			return false, true
		}
		return math.Abs(af-bf) <= 1e-9, true
	}

	ua := unwrapFHIRTyped(a)
	ub := unwrapFHIRTyped(b)
	if as, ok1 := ua.(string); ok1 {
		if bs, ok2 := ub.(string); ok2 {
			return as == bs, true
		}
		return false, true
	}
	if ab, ok1 := ua.(bool); ok1 {
		if bb, ok2 := ub.(bool); ok2 {
			return ab == bb, true
		}
		return false, true
	}
	ma, okMA := ua.(map[string]any)
	mb, okMB := ub.(map[string]any)
	if okMA || okMB {
		if !okMA || !okMB {
			return false, true
		}
		return mapsEqualFHIR(ma, mb, false), true
	}
	return fmt.Sprintf("%v", ua) == fmt.Sprintf("%v", ub), true
}

func mapsEqualFHIR(a, b map[string]any, equiv bool) bool {
	keysA := make([]string, 0, len(a))
	for k := range a {
		if !strings.HasPrefix(k, "_") {
			keysA = append(keysA, k)
		}
	}
	keysB := make([]string, 0, len(b))
	for k := range b {
		if !strings.HasPrefix(k, "_") {
			keysB = append(keysB, k)
		}
	}
	if len(keysA) != len(keysB) {
		return false
	}
	for _, k := range keysA {
		vb, exists := b[k]
		if !exists {
			return false
		}
		va := a[k]
		arrA, isArrA := va.([]any)
		arrB, isArrB := vb.([]any)
		if isArrA || isArrB {
			if !isArrA || !isArrB || len(arrA) != len(arrB) {
				return false
			}
			if equiv {
				if !slicesBijectiveEquivalent(arrA, arrB) {
					return false
				}
			} else {
				for i := range arrA {
					if !fhirPathEqual(arrA[i], arrB[i]) {
						return false
					}
				}
			}
			continue
		}
		if equiv {
			if !fhirPathEquivalent(va, vb) {
				return false
			}
		} else {
			if !fhirPathEqual(va, vb) {
				return false
			}
		}
	}
	return true
}

func fhirPathEqual(a, b any) bool {
	eq, ok := fhirPathEqualTri(a, b)
	return ok && eq
}

func fhirPathEquivalent(a, b any) bool {
	qa, okQA := extractQuantityInfo(a)
	qb, okQB := extractQuantityInfo(b)
	if okQA || okQB {
		if !okQA || !okQB {
			return false
		}
		cmp, ok := compareQuantitiesTri(qa, qb, true)
		return ok && cmp == 0
	}

	ta, okTA := extractTemporalParts(a)
	tb, okTB := extractTemporalParts(b)
	if okTA || okTB {
		if !okTA || !okTB {
			return false
		}
		cmp, ok, _ := compareTemporalsTri(ta, tb)
		return ok && cmp == 0
	}

	af, okFA := toFloat(a)
	bf, okFB := toFloat(b)
	if okFA || okFB {
		if !okFA || !okFB {
			return false
		}
		var dtA, dtB string
		if tvA, ok := a.(fhirTypedValue); ok {
			dtA = tvA.decText
		}
		if tvB, ok := b.(fhirTypedValue); ok {
			dtB = tvB.decText
		}
		return compareFloatWithScale(af, dtA, bf, dtB, true) == 0
	}

	ua := unwrapFHIRTyped(a)
	ub := unwrapFHIRTyped(b)
	if as, ok1 := ua.(string); ok1 {
		if bs, ok2 := ub.(string); ok2 {
			normA := strings.Join(strings.Fields(strings.ToLower(as)), " ")
			normB := strings.Join(strings.Fields(strings.ToLower(bs)), " ")
			return normA == normB
		}
		return false
	}
	ma, okMA := ua.(map[string]any)
	mb, okMB := ub.(map[string]any)
	if okMA || okMB {
		if !okMA || !okMB {
			return false
		}
		return mapsEqualFHIR(ma, mb, true)
	}
	return fhirPathEqual(a, b)
}

func slicesBijectiveEquivalent(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	used := make([]bool, len(b))
	for _, ai := range a {
		matched := false
		for j, bj := range b {
			if !used[j] && fhirPathEquivalent(ai, bj) {
				used[j] = true
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func evalFHIRPathComparison(ctx fhirPathContext, lVals []any, op string, rVals []any) []any {
	switch op {
	case "=":
		if len(lVals) == 0 || len(rVals) == 0 {
			return nil
		}
		if len(lVals) != len(rVals) {
			return []any{false}
		}
		uncertain := false
		for i := range lVals {
			eq, ok := fhirPathEqualTri(lVals[i], rVals[i])
			if !ok {
				uncertain = true
				continue
			}
			if !eq {
				return []any{false}
			}
		}
		if uncertain {
			return nil
		}
		return []any{true}

	case "!=":
		eqRes := evalFHIRPathComparison(ctx, lVals, "=", rVals)
		if len(eqRes) == 0 {
			return nil
		}
		if b, ok := eqRes[0].(bool); ok {
			return []any{!b}
		}
		return nil

	case "~":
		if len(lVals) == 0 && len(rVals) == 0 {
			return []any{true}
		}
		return []any{slicesBijectiveEquivalent(lVals, rVals)}

	case "!~":
		eq := evalFHIRPathComparison(ctx, lVals, "~", rVals)
		if b, ok := singletonBool(eq); ok {
			return []any{!b}
		}
		return []any{true}

	case ">", ">=", "<", "<=":
		if len(lVals) > 1 || len(rVals) > 1 {
			ctx.setExecErr("ordering comparison requires singleton operands")
			return nil
		}
		if len(lVals) != 1 || len(rVals) != 1 {
			return nil
		}
		cmp, ok, incompat := compareOrderedItems(lVals[0], rVals[0])
		if incompat {
			ctx.setExecErr(fmt.Sprintf("incompatible operand types for %q ordering comparison", op))
			return nil
		}
		if !ok {
			return nil
		}
		switch op {
		case "<":
			return []any{cmp < 0}
		case "<=":
			return []any{cmp <= 0}
		case ">":
			return []any{cmp > 0}
		case ">=":
			return []any{cmp >= 0}
		}
	}
	return nil
}

func compareOrderedItems(a, b any) (int, bool, bool) {
	qa, okQA := extractQuantityInfo(a)
	qb, okQB := extractQuantityInfo(b)
	if okQA || okQB {
		if !okQA || !okQB {
			return 0, false, true
		}
		cmp, ok := compareQuantitiesTri(qa, qb, false)
		return cmp, ok, false
	}

	ta, okTA := extractTemporalParts(a)
	tb, okTB := extractTemporalParts(b)
	if okTA || okTB {
		if !okTA || !okTB {
			return 0, false, true
		}
		cmp, ok, diffKind := compareTemporalsTri(ta, tb)
		if diffKind {
			return 0, false, true
		}
		return cmp, ok, false
	}

	af, okFA := toFloat(a)
	bf, okFB := toFloat(b)
	if okFA || okFB {
		if !okFA || !okFB {
			return 0, false, true
		}
		if math.Abs(af-bf) <= 1e-9 {
			return 0, true, false
		}
		if af < bf {
			return -1, true, false
		}
		return 1, true, false
	}

	ua := unwrapFHIRTyped(a)
	ub := unwrapFHIRTyped(b)
	as, okSA := ua.(string)
	bs, okSB := ub.(string)
	if okSA && okSB {
		if as < bs {
			return -1, true, false
		}
		if as > bs {
			return 1, true, false
		}
		return 0, true, false
	}
	return 0, false, true
}

func evalFHIRPathAdditive(ctx fhirPathContext, lVals []any, op string, rVals []any) []any {
	if op == "&" {
		if len(lVals) > 1 || len(rVals) > 1 {
			ctx.setExecErr("'&' string concatenation requires singleton or empty operands")
			return nil
		}
		ls := ""
		if len(lVals) == 1 {
			if s, ok := unwrapFHIRTyped(lVals[0]).(string); ok {
				ls = s
			} else if s, ok := toStringConv(lVals[0]); ok {
				ls = s
			} else {
				ctx.setExecErr("'&' operand cannot be converted to string")
				return nil
			}
		}
		rs := ""
		if len(rVals) == 1 {
			if s, ok := unwrapFHIRTyped(rVals[0]).(string); ok {
				rs = s
			} else if s, ok := toStringConv(rVals[0]); ok {
				rs = s
			} else {
				ctx.setExecErr("'&' operand cannot be converted to string")
				return nil
			}
		}
		return []any{ls + rs}
	}

	if len(lVals) > 1 || len(rVals) > 1 {
		ctx.setExecErr(fmt.Sprintf("additive operator %q requires singleton operands", op))
		return nil
	}
	if len(lVals) != 1 || len(rVals) != 1 {
		return nil
	}

	if res, handled, invalidDur := evalTemporalDurationOp(lVals[0], op, rVals[0]); handled {
		if invalidDur {
			ctx.setExecErr(fmt.Sprintf("invalid temporal duration arithmetic for operator %q", op))
		}
		return res
	}

	if ls, ok1 := unwrapFHIRTyped(lVals[0]).(string); ok1 {
		if rs, ok2 := unwrapFHIRTyped(rVals[0]).(string); ok2 {
			if !isTemporalVal(lVals[0]) && !isTemporalVal(rVals[0]) {
				if op == "+" {
					return []any{ls + rs}
				}
				ctx.setExecErr("'-' subtraction is not valid on string operands")
				return nil
			}
		}
	}

	qa, okQA := extractQuantityInfo(lVals[0])
	qb, okQB := extractQuantityInfo(rVals[0])
	if okQA && okQB {
		ca := effectiveQuantityCode(qa)
		cb := effectiveQuantityCode(qb)
		if ca == cb {
			res := qa.val + qb.val
			if op == "-" {
				res = qa.val - qb.val
			}
			if qv, ok := parseQuantityLiteral(strconv.FormatFloat(res, 'f', -1, 64) + " '" + ca + "'"); ok {
				return []any{qv}
			}
		}
		return nil
	}

	li, lokInt := toIntStrict(lVals[0])
	ri, rokInt := toIntStrict(rVals[0])
	if lokInt && rokInt {
		if op == "+" {
			return []any{li + ri}
		}
		return []any{li - ri}
	}
	lf, lok := toFloat(lVals[0])
	rf, rok := toFloat(rVals[0])
	if lok && rok {
		res := lf + rf
		if op == "-" {
			res = lf - rf
		}
		res = math.Round(res*1e12) / 1e12
		return []any{fhirTypedValue{val: res, fhirType: "System.Decimal"}}
	}
	return nil
}

func isTemporalVal(v any) bool {
	if tv, ok := v.(fhirTypedValue); ok {
		return isTemporalFHIRType(tv.fhirType)
	}
	return false
}

func evalFHIRPathMultiplicative(lVals []any, op string, rVals []any) []any {
	if len(lVals) != 1 || len(rVals) != 1 {
		return nil
	}
	qa, okQA := extractQuantityInfo(lVals[0])
	qb, okQB := extractQuantityInfo(rVals[0])
	if okQA && okQB {
		switch op {
		case "*":
			if qv, ok := multiplyQuantities(qa, qb); ok {
				return []any{qv}
			}
		case "/":
			if qv, ok := divideQuantities(qa, qb); ok {
				return []any{qv}
			}
		}
		return nil
	}

	li, lokInt := toIntStrict(lVals[0])
	ri, rokInt := toIntStrict(rVals[0])
	lf, lok := toFloat(lVals[0])
	rf, rok := toFloat(rVals[0])
	if !lok || !rok {
		return nil
	}
	switch op {
	case "*":
		if lokInt && rokInt {
			return []any{li * ri}
		}
		res := math.Round((lf*rf)*1e12) / 1e12
		return []any{fhirTypedValue{val: res, fhirType: "System.Decimal"}}
	case "/":
		if rf == 0 {
			return nil
		}
		return []any{fhirTypedValue{val: lf / rf, fhirType: "System.Decimal"}}
	case "div":
		if rf == 0 {
			return nil
		}
		return []any{int(math.Trunc(lf / rf))}
	case "mod":
		if rf == 0 {
			return nil
		}
		if lokInt && rokInt {
			return []any{li % ri}
		}
		rem := math.Mod(lf, rf)
		rem = math.Round(rem*1e12) / 1e12
		return []any{fhirTypedValue{val: rem, fhirType: "System.Decimal"}}
	}
	return nil
}

func fhirPathDistinct(items []any) []any {
	var out []any
	for _, it := range items {
		seen := false
		for _, ex := range out {
			if fhirPathEqual(ex, it) {
				seen = true
				break
			}
		}
		if !seen {
			out = append(out, it)
		}
	}
	return out
}
