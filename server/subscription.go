package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/flint-fhir/flint/store/postgres"
)

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
		m["status"] = "active"
		if updated, err := json.Marshal(m); err == nil {
			return updated
		}
	}
	return rawJSON
}

// dispatchSubscriptions evaluates active FHIR R4 Subscriptions in tenant against
// a newly created or updated resource and delivers rest-hook notifications.
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

	for _, subRes := range sr.Matches {
		subJSON, err := s.protoToJSONWithMeta("Subscription", subRes.ResourceProto, subRes.ResVersion, subRes.LastUpdated)
		if err != nil {
			continue
		}
		var subMap map[string]any
		if err := json.Unmarshal(subJSON, &subMap); err != nil {
			continue
		}

		criteria, _ := subMap["criteria"].(string)
		if !subscriptionMatchesResource(criteria, resType, resID, idx) {
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
		if strings.Contains(strings.ToLower(payloadType), "json") {
			if jb, err := s.protoToJSONWithMeta(resType, resourceProto, resVersion, lastUpdated); err == nil {
				bodyBytes = jb
			}
		}

		s.deliverRestHook(ctx, endpoint, payloadType, headers, bodyBytes)
	}
}

func subscriptionMatchesResource(criteria, resType, resID string, idx *postgres.SearchIndexes) bool {
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
			if strings.TrimSpace(part) == resID {
				matchedID = true
				break
			}
		}
		if !matchedID {
			return false
		}
	}

	if idx == nil {
		return len(sp.Tokens) == 0 && len(sp.Strings) == 0 && len(sp.References) == 0
	}

	for spKey, filterVals := range sp.TokenFilters {
		baseName := spKey
		if cIdx := strings.IndexByte(spKey, ':'); cIdx != -1 {
			baseName = spKey[:cIdx]
		}
		for _, rawVal := range filterVals {
			orMatched := false
			for part := range strings.SplitSeq(rawVal, ",") {
				want := strings.TrimSpace(part)
				if want == "" {
					continue
				}
				wantSys := ""
				wantCode := want
				hasPipe := false
				if p := strings.SplitN(want, "|", 2); len(p) == 2 {
					wantSys = p[0]
					wantCode = p[1]
					hasPipe = true
				}
				for _, tok := range idx.Tokens {
					if !strings.EqualFold(tok.SpName, baseName) {
						continue
					}
					if hasPipe {
						if (wantSys == "" || tok.SpSystem == wantSys) && (wantCode == "" || strings.EqualFold(tok.SpValue, wantCode)) {
							orMatched = true
							break
						}
					} else if strings.EqualFold(tok.SpValue, wantCode) {
						orMatched = true
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

	for spKey, filterVals := range sp.StringFilters {
		baseName := spKey
		if cIdx := strings.IndexByte(spKey, ':'); cIdx != -1 {
			baseName = spKey[:cIdx]
		}
		for _, rawVal := range filterVals {
			orMatched := false
			for part := range strings.SplitSeq(rawVal, ",") {
				want := strings.ToLower(strings.TrimSpace(part))
				for _, st := range idx.Strings {
					if strings.EqualFold(st.SpName, baseName) && strings.HasPrefix(strings.ToLower(st.SpValue), want) {
						orMatched = true
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

	for spKey, filterVals := range sp.ReferenceFilters {
		baseName := spKey
		if cIdx := strings.IndexByte(spKey, ':'); cIdx != -1 {
			baseName = spKey[:cIdx]
		}
		for _, rawVal := range filterVals {
			orMatched := false
			for part := range strings.SplitSeq(rawVal, ",") {
				want := strings.TrimSpace(part)
				if sIdx := strings.LastIndexByte(want, '/'); sIdx != -1 {
					want = want[sIdx+1:]
				}
				for _, rf := range idx.References {
					if strings.EqualFold(rf.SpName, baseName) && rf.TargetID == want {
						orMatched = true
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

	return true
}

func (s *Server) deliverRestHook(ctx context.Context, endpoint, payloadType string, headers []string, bodyBytes []byte) {
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	var bodyReader *bytes.Reader
	if len(bodyBytes) > 0 {
		bodyReader = bytes.NewReader(bodyBytes)
	} else {
		bodyReader = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bodyReader)
	if err != nil {
		return
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

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("subscription rest-hook delivery failed", "endpoint", endpoint, "error", err)
		}
		return
	}
	resp.Body.Close()
}
