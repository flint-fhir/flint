package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/flint-fhir/flint/store/postgres"
)

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
		if err := ValidateSubscriptionEndpoint(endpoint, s.disallowPrivateWebhooks); err != nil {
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

		// Dispatch webhook asynchronously via bounded worker semaphore so write latency is never blocked
		asyncCtx := context.WithoutCancel(ctx)
		if s.subSem != nil {
			select {
			case s.subSem <- struct{}{}:
				go func(ep, pt string, hdrs []string, body []byte) {
					defer func() { <-s.subSem }()
					s.deliverRestHook(asyncCtx, ep, pt, hdrs, body)
				}(endpoint, payloadType, headers, bodyBytes)
			default:
				if s.metrics != nil {
					s.metrics.subscriptionDeliveryErrorsTotal.Add(1)
				}
				if s.logger != nil {
					s.logger.Warn("subscription rest-hook queue full; dropping notification", "endpoint", endpoint)
				}
			}
		} else {
			go s.deliverRestHook(asyncCtx, endpoint, payloadType, headers, bodyBytes)
		}
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
	if err := ValidateSubscriptionEndpoint(endpoint, s.disallowPrivateWebhooks); err != nil {
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
			if s.metrics != nil {
				s.metrics.subscriptionsDeliveredTotal.Add(1)
			}
			return
		}
		if resp.StatusCode < 500 {
			break
		}
	}

	if s.metrics != nil {
		s.metrics.subscriptionDeliveryErrorsTotal.Add(1)
	}
	if s.logger != nil {
		s.logger.Warn("subscription rest-hook delivery failed", "endpoint", endpoint, "status", lastStatus, "error", lastErr)
	}
}
