package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/flint-fhir/flint/store/postgres"
)

type subscriptionDeliveryStats struct {
	eventsDelivered int64
	deliveryErrors  int64
	lastDeliveredAt time.Time
	lastError       string
}

func (s *Server) recordSubscriptionDelivery(tenant, subID string, delivered bool, errMsg string) {
	if subID == "" {
		return
	}
	key := tenant + "/" + subID
	s.subStatsMu.Lock()
	defer s.subStatsMu.Unlock()
	if s.subStats == nil {
		s.subStats = make(map[string]*subscriptionDeliveryStats)
	}
	st, ok := s.subStats[key]
	if !ok {
		st = &subscriptionDeliveryStats{}
		s.subStats[key] = st
	}
	if delivered {
		st.eventsDelivered++
		st.lastDeliveredAt = time.Now().UTC()
		st.lastError = ""
	} else {
		st.deliveryErrors++
		st.lastError = errMsg
	}
}

func (s *Server) getSubscriptionDeliveryStats(tenant, subID string) subscriptionDeliveryStats {
	key := tenant + "/" + subID
	s.subStatsMu.RLock()
	defer s.subStatsMu.RUnlock()
	if s.subStats == nil {
		return subscriptionDeliveryStats{}
	}
	if st, ok := s.subStats[key]; ok && st != nil {
		return *st
	}
	return subscriptionDeliveryStats{}
}

// SetDisallowPrivateWebhooks configures whether Subscription rest-hook endpoints
// may target loopback or RFC 1918 private network addresses. Cloud metadata and
// link-local addresses (e.g. 169.254.169.254) are always blocked unconditionally.
func (s *Server) SetDisallowPrivateWebhooks(disallow bool) {
	s.disallowPrivateWebhooks = disallow
}

// ValidateSubscriptionEndpoint validates a FHIR Subscription channel.endpoint URL
// against SSRF attacks (blocking non-HTTP(S) schemes, cloud metadata hosts,
// link-local 169.254.0.0/16 & fe80::/10, unspecified 0.0.0.0/::, multicast, and
// optionally loopback/RFC1918 private ranges when disallowPrivate is true).
func ValidateSubscriptionEndpoint(rawURL string, disallowPrivate bool) error {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return fmt.Errorf("subscription endpoint URL is empty")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("invalid subscription endpoint URL %q: %w", rawURL, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("unsupported subscription endpoint scheme %q: only http and https are allowed", u.Scheme)
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if host == "" {
		return fmt.Errorf("subscription endpoint URL %q is missing hostname", rawURL)
	}
	switch host {
	case "metadata.google.internal", "metadata.goog", "metadata", "instance-data":
		return fmt.Errorf("subscription endpoint hostname %q is blocked by SSRF policy", host)
	}
	if disallowPrivate && (host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".internal")) {
		return fmt.Errorf("subscription endpoint hostname %q is blocked by private network SSRF policy", host)
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if err := validateWebhookIP(addr, disallowPrivate); err != nil {
			return err
		}
	}
	return nil
}

func validateWebhookIP(addr netip.Addr, disallowPrivate bool) error {
	addr = addr.Unmap()
	if addr.IsUnspecified() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() {
		return fmt.Errorf("subscription endpoint IP %s is blocked by SSRF policy (link-local/metadata/unspecified)", addr.String())
	}
	if disallowPrivate && (addr.IsLoopback() || addr.IsPrivate()) {
		return fmt.Errorf("subscription endpoint IP %s is blocked by private/loopback SSRF policy", addr.String())
	}
	return nil
}

func (s *Server) validateSubscriptionResourceJSON(resType string, rawJSON []byte) error {
	if resType != "Subscription" || len(rawJSON) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(rawJSON, &m); err != nil {
		return nil
	}
	if criteria, _ := m["criteria"].(string); strings.TrimSpace(criteria) != "" {
		if err := s.validateSubscriptionCriteria(criteria); err != nil {
			return err
		}
	}
	ch, _ := m["channel"].(map[string]any)
	if ch == nil {
		return nil
	}
	chType, _ := ch["type"].(string)
	endpoint, _ := ch["endpoint"].(string)
	if strings.EqualFold(chType, "rest-hook") && strings.TrimSpace(endpoint) != "" {
		return ValidateSubscriptionEndpoint(endpoint, s.disallowPrivateWebhooks)
	}
	return nil
}

func (s *Server) validateSubscriptionCriteria(criteria string) error {
	criteria = strings.TrimSpace(criteria)
	targetType := criteria
	queryPart := ""
	if qIdx := strings.IndexByte(criteria, '?'); qIdx != -1 {
		targetType = strings.TrimSpace(criteria[:qIdx])
		queryPart = strings.TrimSpace(criteria[qIdx+1:])
	}
	if targetType == "" {
		return fmt.Errorf("subscription criteria is missing target resource type")
	}
	if len(s.protoRegistry) > 0 {
		if _, ok := s.protoRegistry[targetType]; !ok {
			return fmt.Errorf("subscription criteria target resource type %q is not supported", targetType)
		}
	}
	if queryPart != "" {
		vals, err := url.ParseQuery(queryPart)
		if err != nil {
			return fmt.Errorf("invalid subscription criteria query %q: %w", criteria, err)
		}
		if err := postgres.ValidateSearchQuery(vals); err != nil {
			return fmt.Errorf("invalid subscription criteria %q: %w", criteria, err)
		}
	}
	return nil
}

func subscriptionIsExpired(subMap map[string]any, now time.Time) bool {
	endRaw, _ := subMap["end"].(string)
	endRaw = strings.TrimSpace(endRaw)
	if endRaw == "" {
		return false
	}
	dOp, err := postgres.ParseDateOp(endRaw)
	if err != nil {
		return false
	}
	endTime := dOp.High
	if endTime.IsZero() || !endTime.After(dOp.Value) {
		endTime = dOp.Value
	}
	return !endTime.After(now)
}

// normalizeSubscriptionOnWrite transitions a newly requested FHIR R4 Subscription
// ("status": "requested") to "active" per FHIR R4 §2.46.7.1.
func normalizeSubscriptionOnWrite(resType string, rawJSON []byte) []byte {
	if resType != "Subscription" || len(rawJSON) == 0 {
		return rawJSON
	}
	var m map[string]any
	if err := json.Unmarshal(rawJSON, &m); err != nil {
		return rawJSON
	}
	if status, _ := m["status"].(string); strings.EqualFold(status, "requested") {
		if subscriptionIsExpired(m, time.Now().UTC()) {
			m["status"] = "off"
		} else {
			m["status"] = "active"
		}
		if updated, err := json.Marshal(m); err == nil {
			return updated
		}
	}
	return rawJSON
}

// dispatchSubscriptions evaluates active FHIR R4 Subscriptions in tenant against
// a newly created or updated resource and dispatches rest-hook notifications asynchronously.
func (s *Server) dispatchSubscriptions(ctx context.Context, tenant, resType, resID string, resourceProto []byte, resVersion int, lastUpdated time.Time, idx *postgres.SearchIndexes) {
	if s.store == nil || resType == "Subscription" || resType == "AuditEvent" {
		return
	}

	sr, err := s.store.Search(ctx, postgres.SearchParams{
		TenantID: tenant,
		ResType:  "Subscription",
		Tokens:   map[string]string{"status": "active"},
		Count:    100,
	})
	if err != nil || sr == nil || len(sr.Matches) == 0 {
		return
	}

	if idx == nil && len(resourceProto) > 0 {
		if extractor, ok := s.extractors[resType]; ok {
			idx, _ = extractor(tenant, resID, resourceProto)
		}
	}

	now := time.Now().UTC()
	for _, subRes := range sr.Matches {
		subJSON, err := s.protoToJSONWithMeta("Subscription", subRes.ResourceProto, subRes.ResVersion, subRes.LastUpdated)
		if err != nil {
			continue
		}
		var subMap map[string]any
		if err := json.Unmarshal(subJSON, &subMap); err != nil {
			continue
		}

		if subscriptionIsExpired(subMap, now) {
			continue
		}

		criteria, _ := subMap["criteria"].(string)
		if !subscriptionMatchesResourceWithMeta(criteria, resType, resID, lastUpdated, idx) {
			continue
		}

		channel, _ := subMap["channel"].(map[string]any)
		if channel == nil {
			continue
		}
		chType, _ := channel["type"].(string)
		endpoint, _ := channel["endpoint"].(string)
		if !strings.EqualFold(chType, "rest-hook") || strings.TrimSpace(endpoint) == "" {
			continue
		}
		if err := ValidateSubscriptionEndpoint(endpoint, s.disallowPrivateWebhooks); err != nil {
			s.recordSubscriptionDelivery(tenant, subRes.ResID, false, err.Error())
			if s.metrics != nil {
				s.metrics.subscriptionDeliveryErrorsTotal.Add(1)
			}
			if s.logger != nil {
				s.logger.Warn("subscription rest-hook blocked by SSRF check", "endpoint", endpoint, "error", err)
			}
			continue
		}

		payloadType, _ := channel["payload"].(string)
		var headers []string
		if rawHeaders, ok := channel["header"].([]any); ok {
			for _, h := range rawHeaders {
				if hs, ok := h.(string); ok && strings.TrimSpace(hs) != "" {
					headers = append(headers, hs)
				}
			}
		}

		var bodyBytes []byte
		lowerPayload := strings.ToLower(strings.TrimSpace(payloadType))
		switch {
		case lowerPayload == "id-only":
			bodyBytes, _ = json.Marshal(map[string]any{
				"resourceType": resType,
				"id":           resID,
				"meta": map[string]any{
					"versionId":   fmt.Sprintf("%d", resVersion),
					"lastUpdated": lastUpdated.UTC().Format(time.RFC3339),
				},
			})
			payloadType = "application/fhir+json; charset=utf-8"
		case strings.Contains(lowerPayload, "json"):
			if jb, err := s.protoToJSONWithMeta(resType, resourceProto, resVersion, lastUpdated); err == nil {
				bodyBytes = jb
			}
		}

		subID := subRes.ResID
		// Dispatch webhook asynchronously via bounded worker semaphore so write latency is never blocked
		asyncCtx := context.WithoutCancel(ctx)
		if s.subSem != nil {
			select {
			case s.subSem <- struct{}{}:
				go func(sid, ep, pt string, hdrs []string, body []byte) {
					defer func() { <-s.subSem }()
					s.deliverRestHook(asyncCtx, tenant, sid, ep, pt, hdrs, body)
				}(subID, endpoint, payloadType, headers, bodyBytes)
			default:
				s.recordSubscriptionDelivery(tenant, subID, false, "subscription delivery queue full")
				if s.metrics != nil {
					s.metrics.subscriptionDeliveryErrorsTotal.Add(1)
				}
				if s.logger != nil {
					s.logger.Warn("subscription rest-hook queue full; dropping notification", "endpoint", endpoint)
				}
			}
		} else {
			go s.deliverRestHook(asyncCtx, tenant, subID, endpoint, payloadType, headers, bodyBytes)
		}
	}
}

func subscriptionMatchesResourceWithMeta(criteria, resType, resID string, lastUpdated time.Time, idx *postgres.SearchIndexes) bool {
	criteria = strings.TrimSpace(criteria)
	if criteria == "" {
		return false
	}
	targetType := criteria
	queryPart := ""
	if qIdx := strings.IndexByte(criteria, '?'); qIdx != -1 {
		targetType = strings.TrimSpace(criteria[:qIdx])
		queryPart = criteria[qIdx+1:]
	}
	if !strings.EqualFold(targetType, resType) {
		return false
	}
	if queryPart == "" {
		return true
	}
	vals, err := url.ParseQuery(queryPart)
	if err != nil {
		return false
	}
	sp := postgres.BuildSearchParamsFromQuery("", resType, vals)

	if sp.ID != "" {
		matchedID := false
		for part := range strings.SplitSeq(sp.ID, ",") {
			trimmed := strings.TrimSpace(part)
			if sIdx := strings.LastIndexByte(trimmed, '/'); sIdx != -1 {
				trimmed = trimmed[sIdx+1:]
			}
			if trimmed == resID {
				matchedID = true
				break
			}
		}
		if !matchedID {
			return false
		}
	}

	if len(sp.LastUpdated) > 0 && !lastUpdated.IsZero() {
		for _, luOp := range sp.LastUpdated {
			if !matchSubscriptionDateOp(lastUpdated, lastUpdated, luOp) {
				return false
			}
		}
	}

	if idx == nil {
		idx = &postgres.SearchIndexes{}
	}

	// 1. Missing (:missing=true/false)
	for spName, wantMissing := range sp.Missing {
		present := subscriptionIdxHasParam(idx, spName)
		if wantMissing && present {
			return false
		}
		if !wantMissing && !present {
			return false
		}
	}

	// 2. Token filters (supports system|code, |code, system|, :not, multipleOr, multipleAnd)
	tokenFilters := sp.TokenFilters
	if len(tokenFilters) == 0 && len(sp.Tokens) > 0 {
		tokenFilters = make(map[string][]string, len(sp.Tokens))
		for k, v := range sp.Tokens {
			tokenFilters[k] = []string{v}
		}
	}
	for spKey, filterVals := range tokenFilters {
		baseName, modifier := splitSubParamModifier(spKey)
		for _, rawVal := range filterVals {
			orMatched := false
			for part := range strings.SplitSeq(rawVal, ",") {
				want := strings.TrimSpace(part)
				if want == "" {
					continue
				}
				if matchSubscriptionToken(idx.Tokens, baseName, want) {
					orMatched = true
					break
				}
			}
			if modifier == "not" {
				if orMatched {
					return false
				}
			} else if !orMatched {
				return false
			}
		}
	}

	// 3. String filters (supports default prefix, :exact, :contains, multipleOr, multipleAnd)
	stringFilters := sp.StringFilters
	if len(stringFilters) == 0 && len(sp.Strings) > 0 {
		stringFilters = make(map[string][]string, len(sp.Strings))
		for k, v := range sp.Strings {
			stringFilters[k] = []string{v}
		}
	}
	for spKey, filterVals := range stringFilters {
		baseName, modifier := splitSubParamModifier(spKey)
		for _, rawVal := range filterVals {
			orMatched := false
			for part := range strings.SplitSeq(rawVal, ",") {
				want := strings.ToLower(strings.TrimSpace(part))
				if want == "" {
					continue
				}
				for _, st := range idx.Strings {
					if !strings.EqualFold(st.SpName, baseName) {
						continue
					}
					cand := strings.ToLower(st.SpValue)
					switch modifier {
					case "exact":
						if cand == want {
							orMatched = true
						}
					case "contains":
						if strings.Contains(cand, want) {
							orMatched = true
						}
					default:
						if strings.HasPrefix(cand, want) {
							orMatched = true
						}
					}
					if orMatched {
						break
					}
				}
				if orMatched {
					break
				}
			}
			if !orMatched {
				return false
			}
		}
	}

	// 4. Reference filters (supports TargetType/id, :TargetType modifier, and bare id)
	referenceFilters := sp.ReferenceFilters
	if len(referenceFilters) == 0 && len(sp.References) > 0 {
		referenceFilters = make(map[string][]string, len(sp.References))
		for k, v := range sp.References {
			referenceFilters[k] = []string{v}
		}
	}
	for spKey, filterVals := range referenceFilters {
		baseName, modType := splitSubParamModifier(spKey)
		for _, rawVal := range filterVals {
			orMatched := false
			for part := range strings.SplitSeq(rawVal, ",") {
				want := strings.TrimSpace(part)
				if want == "" {
					continue
				}
				wantType := modType
				wantID := want
				if parts := strings.SplitN(want, "/", 2); len(parts) == 2 {
					wantType = parts[0]
					wantID = parts[1]
				}
				for _, rf := range idx.References {
					if !strings.EqualFold(rf.SpName, baseName) {
						continue
					}
					if rf.TargetID != wantID {
						continue
					}
					if wantType != "" && rf.TargetType != "" && !strings.EqualFold(rf.TargetType, wantType) {
						continue
					}
					orMatched = true
					break
				}
				if orMatched {
					break
				}
			}
			if !orMatched {
				return false
			}
		}
	}

	// 5. Date filters (supports eq, ne, lt, eb, le, gt, sa, ge, ap and multi-bound ranges)
	dateFilters := sp.DateFilters
	if len(dateFilters) == 0 && len(sp.Dates) > 0 {
		dateFilters = make(map[string][]postgres.DateOp, len(sp.Dates))
		for k, v := range sp.Dates {
			dateFilters[k] = []postgres.DateOp{v}
		}
	}
	for spName, ops := range dateFilters {
		if len(ops) == 0 {
			continue
		}
		matchedRow := false
		for _, dt := range idx.Dates {
			if !strings.EqualFold(dt.SpName, spName) {
				continue
			}
			allBoundsOK := true
			for _, op := range ops {
				if !matchSubscriptionDateOp(dt.SpLow, dt.SpHigh, op) {
					allBoundsOK = false
					break
				}
			}
			if allBoundsOK {
				matchedRow = true
				break
			}
		}
		if !matchedRow {
			return false
		}
	}

	// 6. Quantity filters (supports eq, ne, lt, le, gt, ge + system/code and multi-bound ranges)
	quantityFilters := sp.QuantityFilters
	if len(quantityFilters) == 0 && len(sp.Quantities) > 0 {
		quantityFilters = make(map[string][]postgres.QuantityOp, len(sp.Quantities))
		for k, v := range sp.Quantities {
			quantityFilters[k] = []postgres.QuantityOp{v}
		}
	}
	for spName, ops := range quantityFilters {
		if len(ops) == 0 {
			continue
		}
		matchedRow := false
		for _, qt := range idx.Quantities {
			if !strings.EqualFold(qt.SpName, spName) {
				continue
			}
			allBoundsOK := true
			for _, op := range ops {
				if !matchSubscriptionQuantityOp(qt, op) {
					allBoundsOK = false
					break
				}
			}
			if allBoundsOK {
				matchedRow = true
				break
			}
		}
		if !matchedRow {
			return false
		}
	}

	// 7. URI filters (supports exact, :below, :above across spidx_uri and spidx_token)
	uriFilters := sp.URIFilters
	if len(uriFilters) == 0 && len(sp.URIs) > 0 {
		uriFilters = make(map[string][]string, len(sp.URIs))
		for k, v := range sp.URIs {
			uriFilters[k] = []string{v}
		}
	}
	for spKey, filterVals := range uriFilters {
		baseName, modifier := splitSubParamModifier(spKey)
		var candidates []string
		for _, u := range idx.URIs {
			if strings.EqualFold(u.SpName, baseName) {
				candidates = append(candidates, u.SpValue)
			}
		}
		for _, t := range idx.Tokens {
			if strings.EqualFold(t.SpName, baseName) {
				candidates = append(candidates, t.SpValue)
			}
		}
		for _, rawVal := range filterVals {
			orMatched := false
			for part := range strings.SplitSeq(rawVal, ",") {
				v := strings.TrimSpace(part)
				if v == "" {
					continue
				}
				for _, cand := range candidates {
					switch modifier {
					case "below":
						baseURI := strings.TrimSuffix(v, "/")
						if cand == v || cand == baseURI || strings.HasPrefix(cand, baseURI+"/") {
							orMatched = true
						}
					case "above":
						baseCand := strings.TrimSuffix(cand, "/")
						baseV := strings.TrimSuffix(v, "/")
						if cand == v || baseCand == baseV || strings.HasPrefix(baseV, baseCand+"/") {
							orMatched = true
						}
					default:
						if cand == v {
							orMatched = true
						}
					}
					if orMatched {
						break
					}
				}
				if orMatched {
					break
				}
			}
			if !orMatched {
				return false
			}
		}
	}

	// 8. Composite token$quantity filters (code-value-quantity, component-code-value-quantity, combo-code-value-quantity)
	for _, orGroup := range sp.CompositeTokenQuantities {
		groupMatched := false
		for _, cp := range orGroup {
			if matchSubscriptionCompositeTokenQuantity(idx, cp) {
				groupMatched = true
				break
			}
		}
		if len(orGroup) > 0 && !groupMatched {
			return false
		}
	}

	return true
}

func splitSubParamModifier(spKey string) (string, string) {
	if cIdx := strings.IndexByte(spKey, ':'); cIdx != -1 {
		return spKey[:cIdx], spKey[cIdx+1:]
	}
	return spKey, ""
}

func subscriptionIdxHasParam(idx *postgres.SearchIndexes, spName string) bool {
	if idx == nil {
		return false
	}
	for _, t := range idx.Tokens {
		if strings.EqualFold(t.SpName, spName) {
			return true
		}
	}
	for _, s := range idx.Strings {
		if strings.EqualFold(s.SpName, spName) {
			return true
		}
	}
	for _, d := range idx.Dates {
		if strings.EqualFold(d.SpName, spName) {
			return true
		}
	}
	for _, q := range idx.Quantities {
		if strings.EqualFold(q.SpName, spName) {
			return true
		}
	}
	for _, r := range idx.References {
		if strings.EqualFold(r.SpName, spName) {
			return true
		}
	}
	for _, u := range idx.URIs {
		if strings.EqualFold(u.SpName, spName) {
			return true
		}
	}
	return false
}

func matchSubscriptionToken(tokens []postgres.SpidxToken, baseName, want string) bool {
	wantSys := ""
	wantCode := want
	hasPipe := false
	if p := strings.SplitN(want, "|", 2); len(p) == 2 {
		wantSys = p[0]
		wantCode = p[1]
		hasPipe = true
	}
	normWantCode := strings.ToLower(strings.ReplaceAll(wantCode, "_", "-"))
	for _, tok := range tokens {
		if !strings.EqualFold(tok.SpName, baseName) {
			continue
		}
		normTokVal := strings.ToLower(strings.ReplaceAll(tok.SpValue, "_", "-"))
		if hasPipe {
			if wantSys == "" {
				if tok.SpSystem == "" && (strings.EqualFold(tok.SpValue, wantCode) || normTokVal == normWantCode) {
					return true
				}
			} else if wantCode == "" {
				if tok.SpSystem == wantSys {
					return true
				}
			} else if (tok.SpSystem == wantSys || tok.SpSystem == "") && (strings.EqualFold(tok.SpValue, wantCode) || normTokVal == normWantCode) {
				return true
			}
		} else if strings.EqualFold(tok.SpValue, wantCode) || normTokVal == normWantCode {
			return true
		}
	}
	return false
}

func matchSubscriptionDateOp(spLow, spHigh time.Time, op postgres.DateOp) bool {
	low := op.Value
	high := op.High
	if high.IsZero() || !high.After(low) {
		high = low
	}
	if spHigh.IsZero() || spHigh.Before(spLow) {
		spHigh = spLow
	}
	hasInterval := high.After(low)
	switch op.Prefix {
	case "lt", "eb":
		return spLow.Before(low)
	case "le":
		if hasInterval {
			return spLow.Before(high)
		}
		return !spLow.After(low)
	case "gt", "sa":
		if hasInterval {
			return !spHigh.Before(high)
		}
		return spHigh.After(low)
	case "ge":
		return !spHigh.Before(low)
	case "ne":
		if hasInterval {
			return spHigh.Before(low) || !spLow.Before(high)
		}
		return !spLow.Equal(low) || !spHigh.Equal(low)
	case "ap":
		approxLow := low.Add(-24 * time.Hour)
		approxHigh := high.Add(24 * time.Hour)
		return !spLow.After(approxHigh) && !spHigh.Before(approxLow)
	default: // "eq"
		if hasInterval {
			return spLow.Before(high) && !spHigh.Before(low)
		}
		return !spLow.After(low) && !spHigh.Before(low)
	}
}

func matchSubscriptionQuantityOp(qt postgres.SpidxQuantity, op postgres.QuantityOp) bool {
	if op.System != "" && qt.SpSystem != op.System {
		return false
	}
	if op.Code != "" && !strings.EqualFold(qt.SpCode, op.Code) {
		return false
	}
	switch op.Prefix {
	case "ne":
		return math.Abs(qt.SpValue-op.Value) > 1e-6
	case "lt":
		return qt.SpValue < op.Value
	case "le":
		return qt.SpValue <= op.Value
	case "gt":
		return qt.SpValue > op.Value
	case "ge":
		return qt.SpValue >= op.Value
	default: // "eq"
		return math.Abs(qt.SpValue-op.Value) <= 1e-6
	}
}

func matchSubscriptionCompositeTokenQuantity(idx *postgres.SearchIndexes, cp postgres.CompositeTokenQuantityParam) bool {
	matchComp := func() bool {
		compKey := "comp-cvq:" + cp.TokenCode
		if cp.TokenSystem != "" {
			compKey = "comp-cvq:" + cp.TokenSystem + "|" + cp.TokenCode
		}
		for _, qt := range idx.Quantities {
			matchedName := strings.EqualFold(qt.SpName, compKey)
			if !matchedName && cp.TokenSystem == "" && strings.HasPrefix(qt.SpName, "comp-cvq:") && strings.HasSuffix(strings.ToLower(qt.SpName), "|"+strings.ToLower(cp.TokenCode)) {
				matchedName = true
			}
			if matchedName && matchSubscriptionQuantityOp(qt, cp.Quantity) {
				return true
			}
		}
		return false
	}
	matchTop := func() bool {
		wantTok := cp.TokenCode
		if cp.TokenSystem != "" {
			wantTok = cp.TokenSystem + "|" + cp.TokenCode
		}
		if !matchSubscriptionToken(idx.Tokens, "code", wantTok) {
			return false
		}
		for _, qt := range idx.Quantities {
			if strings.EqualFold(qt.SpName, "value-quantity") && matchSubscriptionQuantityOp(qt, cp.Quantity) {
				return true
			}
		}
		return false
	}
	switch cp.ParamName {
	case "component-code-value-quantity":
		return matchComp()
	case "combo-code-value-quantity":
		return matchComp() || matchTop()
	default:
		return matchTop()
	}
}

func (s *Server) deliverRestHook(ctx context.Context, tenant, subID, endpoint, payloadType string, headers []string, bodyBytes []byte) {
	if err := ValidateSubscriptionEndpoint(endpoint, s.disallowPrivateWebhooks); err != nil {
		s.recordSubscriptionDelivery(tenant, subID, false, err.Error())
		if s.metrics != nil {
			s.metrics.subscriptionDeliveryErrorsTotal.Add(1)
		}
		return
	}

	disallowPrivate := s.disallowPrivateWebhooks
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	transport := &http.Transport{
		DialContext: func(dialCtx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupNetIP(dialCtx, "ip", host)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				if err := validateWebhookIP(ip, disallowPrivate); err != nil {
					return nil, err
				}
			}
			if len(ips) > 0 {
				return dialer.DialContext(dialCtx, network, net.JoinHostPort(ips[0].String(), port))
			}
			return dialer.DialContext(dialCtx, network, addr)
		},
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	const maxAttempts = 2
	var lastErr error
	var lastStatus int

	for attempt := range maxAttempts {
		if attempt > 0 {
			time.Sleep(50 * time.Millisecond)
		}
		reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)

		var bodyReader *bytes.Reader
		if len(bodyBytes) > 0 {
			bodyReader = bytes.NewReader(bodyBytes)
		} else {
			bodyReader = bytes.NewReader(nil)
		}

		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bodyReader)
		if err != nil {
			cancel()
			lastErr = err
			break
		}
		if len(bodyBytes) > 0 {
			ct := strings.TrimSpace(payloadType)
			if ct == "" {
				ct = "application/fhir+json; charset=utf-8"
			}
			req.Header.Set("Content-Type", ct)
		}
		for _, h := range headers {
			if parts := strings.SplitN(h, ":", 2); len(parts) == 2 {
				req.Header.Add(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]))
			}
		}

		resp, err := client.Do(req)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()
		lastStatus = resp.StatusCode
		lastErr = nil
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			s.recordSubscriptionDelivery(tenant, subID, true, "")
			if s.metrics != nil {
				s.metrics.subscriptionsDeliveredTotal.Add(1)
			}
			return
		}
		if resp.StatusCode < 500 {
			break
		}
	}

	errMsg := fmt.Sprintf("HTTP %d", lastStatus)
	if lastErr != nil {
		errMsg = lastErr.Error()
	}
	s.recordSubscriptionDelivery(tenant, subID, false, errMsg)
	if s.metrics != nil {
		s.metrics.subscriptionDeliveryErrorsTotal.Add(1)
	}
	if s.logger != nil {
		s.logger.Warn("subscription rest-hook delivery failed", "endpoint", endpoint, "status", lastStatus, "error", lastErr)
	}
}

// handleSubscriptionStatus handles GET/POST /fhir/r4/{tenant}/Subscription/{id}/$status
// and GET/POST /fhir/r4/{tenant}/Subscription/$status, returning a FHIR Parameters status report
// (or a searchset Bundle of Parameters when invoked at type level without a specific id).
func (s *Server) handleSubscriptionStatus(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	if resType := r.PathValue("resourceType"); resType != "" && resType != "Subscription" {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("$status is only supported on Subscription resources (got %s)", resType))
		return
	}
	subID := strings.TrimSpace(r.PathValue("id"))
	if subID == "" {
		subID = strings.TrimSpace(r.URL.Query().Get("id"))
	}
	if subID == "" && r.Method == http.MethodPost && r.Body != nil && r.ContentLength != 0 {
		if body, err := io.ReadAll(io.LimitReader(r.Body, 1*1024*1024)); err == nil && len(bytes.TrimSpace(body)) > 0 {
			var raw map[string]any
			if json.Unmarshal(body, &raw) == nil {
				if paramsArr, ok := raw["parameter"].([]any); ok {
					for _, p := range paramsArr {
						pm, _ := p.(map[string]any)
						name, _ := pm["name"].(string)
						if name == "id" || name == "subscription" {
							if sVal, ok := pm["valueString"].(string); ok && strings.TrimSpace(sVal) != "" {
								subID = cleanID(sVal)
								break
							}
							if cVal, ok := pm["valueId"].(string); ok && strings.TrimSpace(cVal) != "" {
								subID = cleanID(cVal)
								break
							}
							if refMap, ok := pm["valueReference"].(map[string]any); ok {
								if refStr, ok := refMap["reference"].(string); ok && strings.TrimSpace(refStr) != "" {
									subID = cleanID(refStr)
									break
								}
							}
						}
					}
				}
			}
		}
	}

	if s.store == nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "operational store is not configured")
		return
	}

	now := time.Now().UTC()

	if subID != "" {
		rec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, "Subscription", subID)
		if errors.Is(err, sql.ErrNoRows) {
			writeOperationOutcome(w, http.StatusNotFound, "not-found",
				fmt.Sprintf("Subscription/%s not found", subID))
			return
		}
		if errors.Is(err, postgres.ErrResourceDeleted) {
			writeOperationOutcome(w, http.StatusGone, "deleted",
				fmt.Sprintf("Subscription/%s has been deleted", subID))
			return
		}
		if err != nil {
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to read Subscription")
			return
		}
		statusParams, err := s.buildSubscriptionStatusParameters(tenant, subID, rec.ResourceProto, rec.ResVersion, rec.LastUpdated, now)
		if err != nil {
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to format Subscription status")
			return
		}
		w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(statusParams)
		return
	}

	sr, err := s.store.Search(r.Context(), postgres.SearchParams{
		TenantID: tenant,
		ResType:  "Subscription",
		Count:    100,
	})
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to search Subscriptions")
		return
	}

	entries := make([]map[string]any, 0, len(sr.Matches))
	for _, m := range sr.Matches {
		statusParams, err := s.buildSubscriptionStatusParameters(tenant, m.ResID, m.ResourceProto, m.ResVersion, m.LastUpdated, now)
		if err != nil {
			continue
		}
		entries = append(entries, map[string]any{
			"fullUrl":  fmt.Sprintf("Subscription/%s/$status", m.ResID),
			"resource": statusParams,
			"search": map[string]any{
				"mode": "match",
			},
		})
	}

	bundle := map[string]any{
		"resourceType": "Bundle",
		"type":         "searchset",
		"total":        len(entries),
		"entry":        entries,
	}
	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(bundle)
}

func (s *Server) buildSubscriptionStatusParameters(
	tenant, subID string,
	resourceProto []byte,
	resVersion int,
	lastUpdated time.Time,
	now time.Time,
) (map[string]any, error) {
	subJSON, err := s.protoToJSONWithMeta("Subscription", resourceProto, resVersion, lastUpdated)
	if err != nil {
		return nil, err
	}
	var subMap map[string]any
	if err := json.Unmarshal(subJSON, &subMap); err != nil {
		return nil, err
	}

	status, _ := subMap["status"].(string)
	if subscriptionIsExpired(subMap, now) {
		status = "off"
	}
	criteria, _ := subMap["criteria"].(string)
	subErr, _ := subMap["error"].(string)

	var chType, endpoint string
	if ch, ok := subMap["channel"].(map[string]any); ok {
		chType, _ = ch["type"].(string)
		endpoint, _ = ch["endpoint"].(string)
	}

	stats := s.getSubscriptionDeliveryStats(tenant, subID)
	effectiveErr := strings.TrimSpace(stats.lastError)
	if effectiveErr == "" {
		effectiveErr = strings.TrimSpace(subErr)
	}

	params := []map[string]any{
		{
			"name": "subscription",
			"valueReference": map[string]any{
				"reference": "Subscription/" + subID,
			},
		},
		{
			"name":      "status",
			"valueCode": status,
		},
		{
			"name":      "type",
			"valueCode": "query-status",
		},
		{
			"name":        "events-since-subscription-start",
			"valueString": strconv.FormatInt(stats.eventsDelivered, 10),
		},
		{
			"name":         "events-in-notification",
			"valueInteger": 0,
		},
	}
	if criteria != "" {
		params = append(params, map[string]any{
			"name":        "criteria",
			"valueString": criteria,
		})
	}
	if chType != "" {
		params = append(params, map[string]any{
			"name":      "channel-type",
			"valueCode": chType,
		})
	}
	if endpoint != "" {
		params = append(params, map[string]any{
			"name":     "channel-endpoint",
			"valueUri": endpoint,
		})
	}
	if !stats.lastDeliveredAt.IsZero() {
		params = append(params, map[string]any{
			"name":         "last-delivered-at",
			"valueInstant": stats.lastDeliveredAt.Format(time.RFC3339),
		})
	}
	if stats.deliveryErrors > 0 {
		params = append(params, map[string]any{
			"name":         "delivery-errors",
			"valueInteger": stats.deliveryErrors,
		})
	}
	if effectiveErr != "" {
		params = append(params, map[string]any{
			"name":        "error",
			"valueString": effectiveErr,
		})
	}

	return map[string]any{
		"resourceType": "Parameters",
		"id":           subID + "-status",
		"parameter":    params,
	}, nil
}
