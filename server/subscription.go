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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/pkg/fhirutil"
	"github.com/flint-fhir/flint/store"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"
)

const (
	backportSubscriptionProfileURL        = "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-subscription"
	backportNotificationBundleProfileURL  = "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-subscription-notification-r4"
	backportSubscriptionStatusProfileURL  = "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-subscription-status-r4"
	backportFilterCriteriaExtURL          = "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-filter-criteria"
	backportCapStatementTopicCanonicalURL = "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/capabilitystatement-subscriptiontopic-canonical"
)

var supportedBackportTopics = map[string]string{
	"https://inferno.healthit.gov/suites/custom/subscriptions_r5_backport_r4_client/topics/patient-admission": "Encounter",
	"http://inferno.healthit.gov/suites/custom/subscriptions_r5_backport_r4_client/topics/patient-admission":  "Encounter",
	"http://example.org/FHIR/SubscriptionTopic/encounter-complete":                                            "Encounter",
	"http://example.org/FHIR/R5/SubscriptionTopic/encounter-start":                                            "Encounter",
	"http://hl7.org/fhir/uv/subscriptions-backport/SubscriptionTopic/r5-encounter-complete":                   "Encounter",
	"http://hl7.org/SubscriptionTopic/admission":                                                              "Encounter",
	"http://example.org/FHIR/SubscriptionTopic/observation-vital-signs":                                       "Observation",
}

func (s *Server) buildSubscriptionTopicExtensions() []map[string]any {
	topics := make([]string, 0, len(supportedBackportTopics))
	for k := range supportedBackportTopics {
		topics = append(topics, k)
	}
	sort.Strings(topics)
	exts := make([]map[string]any, 0, len(topics))
	for _, t := range topics {
		exts = append(exts, map[string]any{
			"url":            backportCapStatementTopicCanonicalURL,
			"valueCanonical": t,
		})
	}
	return exts
}

func (s *Server) recordTenantFHIRBaseURL(r *http.Request, tenant string) {
	if r == nil || strings.TrimSpace(tenant) == "" {
		return
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := strings.TrimSpace(r.Host)
	if fwdHost := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); fwdHost != "" {
		host = fwdHost
	}
	if host == "" {
		return
	}
	base := fmt.Sprintf("%s://%s/fhir/r4/%s", scheme, host, strings.TrimSpace(tenant))
	s.subBaseURLMu.Lock()
	if s.subTenantBaseURL == nil {
		s.subTenantBaseURL = make(map[string]string)
	}
	s.subTenantBaseURL[tenant] = base
	s.subBaseURLMu.Unlock()
}

func (s *Server) resolveTenantFHIRBaseURL(tenant string) string {
	s.subBaseURLMu.RLock()
	base := s.subTenantBaseURL[tenant]
	s.subBaseURLMu.RUnlock()
	if base != "" {
		return strings.TrimRight(base, "/")
	}
	if s.smartConfig != nil && strings.TrimSpace(s.smartConfig.Issuer) != "" {
		return strings.TrimRight(strings.TrimSpace(s.smartConfig.Issuer), "/")
	}
	if strings.TrimSpace(tenant) == "" {
		tenant = "default"
	}
	return "http://127.0.0.1:8080/fhir/r4/" + tenant
}

func isBackportSubscription(subMap map[string]any) bool {
	if subMap == nil {
		return false
	}
	if meta, ok := subMap["meta"].(map[string]any); ok {
		if profs, ok := meta["profile"].([]any); ok {
			for _, p := range profs {
				if ps, _ := p.(string); strings.Contains(ps, "subscriptions-backport") {
					return true
				}
			}
		}
	}
	if criteria, _ := subMap["criteria"].(string); strings.HasPrefix(strings.TrimSpace(criteria), "http://") || strings.HasPrefix(strings.TrimSpace(criteria), "https://") {
		return true
	}
	return false
}

func findCrossVersionExtensionURL(v any) string {
	switch val := v.(type) {
	case map[string]any:
		for k, child := range val {
			if k == "extension" {
				if arr, ok := child.([]any); ok {
					for _, it := range arr {
						if em, ok := it.(map[string]any); ok {
							if u, _ := em["url"].(string); isCrossVersionFHIRExtensionURL(u) {
								return u
							}
						}
					}
				}
			}
			if found := findCrossVersionExtensionURL(child); found != "" {
				return found
			}
		}
	case []any:
		for _, child := range val {
			if found := findCrossVersionExtensionURL(child); found != "" {
				return found
			}
		}
	}
	return ""
}

func isCrossVersionFHIRExtensionURL(rawURL string) bool {
	u := strings.TrimSpace(rawURL)
	for _, prefix := range []string{"http://hl7.org/fhir/", "https://hl7.org/fhir/"} {
		if rest, ok := strings.CutPrefix(u, prefix); ok {
			slash := strings.IndexByte(rest, '/')
			if slash > 0 {
				verSeg := rest[:slash]
				if dot := strings.IndexByte(verSeg, '.'); dot > 0 && dot < len(verSeg)-1 {
					allDigits := true
					for i := range len(verSeg) {
						if i == dot {
							continue
						}
						if verSeg[i] < '0' || verSeg[i] > '9' {
							allDigits = false
							break
						}
					}
					if allDigits {
						return true
					}
				}
			}
		}
	}
	return false
}

func extractBackportFilterCriteria(subMap map[string]any) []string {
	var out []string
	critMeta, _ := subMap["_criteria"].(map[string]any)
	if critMeta == nil {
		return nil
	}
	exts, _ := critMeta["extension"].([]any)
	for _, it := range exts {
		em, _ := it.(map[string]any)
		u, _ := em["url"].(string)
		if u == backportFilterCriteriaExtURL || strings.HasSuffix(u, "/backport-filter-criteria") {
			if vs, _ := em["valueString"].(string); strings.TrimSpace(vs) != "" {
				out = append(out, strings.TrimSpace(vs))
			}
		}
	}
	return out
}

func normalizeBackportFilterString(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.Contains(trimmed, "?") {
		return trimmed
	}
	eqIdx := strings.IndexByte(trimmed, '=')
	dotIdx := strings.IndexByte(trimmed, '.')
	if dotIdx > 0 && (eqIdx == -1 || dotIdx < eqIdx) {
		return trimmed[:dotIdx] + "?" + trimmed[dotIdx+1:]
	}
	return trimmed
}

type subscriptionEventRecord struct {
	EventNumber int64
	Timestamp   time.Time
	ResType     string
	ResID       string
	ResVersion  int
	Action      string
	Resource    map[string]any
}

type subscriptionDeliveryStats struct {
	eventsDelivered int64
	deliveryErrors  int64
	lastDeliveredAt time.Time
	lastError       string
	events          []subscriptionEventRecord
}

func (s *Server) recordSubscriptionEventWithAction(tenant, subID, resType, resID string, resVersion int, lastUpdated time.Time, resourceProto []byte, action string) subscriptionEventRecord {
	if subID == "" {
		return subscriptionEventRecord{}
	}
	if action == "" {
		if resVersion <= 1 {
			action = "create"
		} else {
			action = "update"
		}
	}
	ts := lastUpdated.UTC()
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	var resMap map[string]any
	if action != "delete" && len(resourceProto) > 0 {
		if m, err := s.protoToFHIRMapWithMeta(resType, resourceProto, resVersion, ts); err == nil {
			resMap = m
		}
	}
	if resMap == nil && action != "delete" {
		resMap = map[string]any{
			"resourceType": resType,
			"id":           resID,
			"meta": map[string]any{
				"versionId":   strconv.Itoa(resVersion),
				"lastUpdated": ts.Format(time.RFC3339),
			},
		}
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
	nextNum := int64(1)
	if n := len(st.events); n > 0 {
		nextNum = st.events[n-1].EventNumber + 1
	}
	ev := subscriptionEventRecord{
		EventNumber: nextNum,
		Timestamp:   ts,
		ResType:     resType,
		ResID:       resID,
		ResVersion:  resVersion,
		Action:      action,
		Resource:    resMap,
	}
	st.events = append(st.events, ev)
	if len(st.events) > 500 {
		st.events = st.events[len(st.events)-500:]
	}
	return ev
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
		cp := *st
		if len(st.events) > 0 {
			cp.events = make([]subscriptionEventRecord, len(st.events))
			copy(cp.events, st.events)
		}
		return cp
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
	if badExt := findCrossVersionExtensionURL(m); badExt != "" {
		return fmt.Errorf("unsupported cross-version extension %q on R4 Subscription", badExt)
	}
	criteria, _ := m["criteria"].(string)
	trimmedCriteria := strings.TrimSpace(criteria)
	expectedTopicResType := ""
	if trimmedCriteria != "" {
		if strings.HasPrefix(trimmedCriteria, "http://") || strings.HasPrefix(trimmedCriteria, "https://") {
			rt, ok := supportedBackportTopics[trimmedCriteria]
			if !ok {
				return fmt.Errorf("unsupported SubscriptionTopic canonical URL %q", trimmedCriteria)
			}
			expectedTopicResType = rt
		} else {
			if err := s.validateSubscriptionCriteria(trimmedCriteria); err != nil {
				return err
			}
		}
	}
	for _, rawFilter := range extractBackportFilterCriteria(m) {
		if err := s.validateBackportFilterCriteria(expectedTopicResType, rawFilter); err != nil {
			return err
		}
	}

	ch, _ := m["channel"].(map[string]any)
	if ch == nil {
		return nil
	}
	chType := strings.ToLower(strings.TrimSpace(stringField(ch, "type")))
	if chType != "" && chType != "rest-hook" && chType != "websocket" {
		return fmt.Errorf("unsupported Subscription channel.type %q: only rest-hook and websocket are supported", chType)
	}
	rawPayload := strings.TrimSpace(stringField(ch, "payload"))
	mediaPayload := strings.ToLower(rawPayload)
	if semi := strings.IndexByte(mediaPayload, ';'); semi >= 0 {
		mediaPayload = strings.TrimSpace(mediaPayload[:semi])
	}
	switch mediaPayload {
	case "", "application/fhir+json", "application/json", "application/fhir+xml", "application/xml",
		"application/cloudevents+json", "application/cloudevents-batch+json", "application/x-protobuf",
		"empty", "id-only", "full-resource":
	default:
		return fmt.Errorf("unsupported Subscription channel.payload MIME type %q", rawPayload)
	}
	if chType == "websocket" && (mediaPayload == "application/cloudevents+json" || mediaPayload == "application/cloudevents-batch+json") {
		return fmt.Errorf("unsupported Subscription channel.type %q and payload %q combination", chType, rawPayload)
	}
	if payloadMeta, ok := ch["_payload"].(map[string]any); ok {
		if exts, ok := payloadMeta["extension"].([]any); ok {
			for _, it := range exts {
				em, _ := it.(map[string]any)
				u, _ := em["url"].(string)
				if u == backportPayloadContentExtURL || strings.HasSuffix(u, "/backport-payload-content") {
					vc := strings.ToLower(strings.TrimSpace(stringField(em, "valueCode")))
					if vc == "" {
						vc = strings.ToLower(strings.TrimSpace(stringField(em, "valueString")))
					}
					if vc != "empty" && vc != "id-only" && vc != "full-resource" {
						return fmt.Errorf("unsupported backport-payload-content code %q", vc)
					}
				}
			}
		}
	}
	endpoint := strings.TrimSpace(stringField(ch, "endpoint"))
	if chType == "rest-hook" && (endpoint != "" || isBackportSubscription(m)) {
		return ValidateSubscriptionEndpoint(endpoint, s.disallowPrivateWebhooks)
	}
	return nil
}

func (s *Server) validateBackportFilterCriteria(expectedResType, rawFilter string) error {
	norm := normalizeBackportFilterString(rawFilter)
	qIdx := strings.IndexByte(norm, '?')
	targetType := strings.TrimSpace(norm)
	queryPart := ""
	if qIdx != -1 {
		targetType = strings.TrimSpace(norm[:qIdx])
		queryPart = strings.TrimSpace(norm[qIdx+1:])
	}
	if targetType == "" {
		return fmt.Errorf("invalid backport-filter-criteria %q: missing resource type", rawFilter)
	}
	if expectedResType != "" && !strings.EqualFold(targetType, expectedResType) {
		return fmt.Errorf("backport-filter-criteria resource %q does not match topic target %q", targetType, expectedResType)
	}
	if len(s.protoRegistry) > 0 {
		if _, ok := s.protoRegistry[targetType]; !ok {
			return fmt.Errorf("backport-filter-criteria target resource type %q is not supported", targetType)
		}
	}
	if queryPart == "" {
		return fmt.Errorf("backport-filter-criteria %q is missing search filter parameters", rawFilter)
	}
	vals, err := url.ParseQuery(queryPart)
	if err != nil {
		return fmt.Errorf("invalid backport-filter-criteria %q: %w", rawFilter, err)
	}
	if err := store.ValidateSearchQuery(vals); err != nil {
		return fmt.Errorf("unsupported search parameter in backport-filter-criteria %q: %w", rawFilter, err)
	}
	allowedDefs := resourceSearchParamDefs()[targetType]
	for rawKey := range vals {
		baseKey := rawKey
		if cIdx := strings.IndexByte(baseKey, ':'); cIdx != -1 {
			baseKey = baseKey[:cIdx]
		}
		if dIdx := strings.IndexByte(baseKey, '.'); dIdx != -1 {
			baseKey = baseKey[:dIdx]
		}
		baseKey = strings.TrimSpace(baseKey)
		if baseKey == "_id" || baseKey == "_lastUpdated" || baseKey == "_tag" || baseKey == "_profile" || baseKey == "_security" {
			continue
		}
		matchedDef := false
		for _, def := range allowedDefs {
			if def["name"] == baseKey {
				matchedDef = true
				break
			}
		}
		if !matchedDef {
			return fmt.Errorf("unsupported search parameter %q for resource %s in backport-filter-criteria %q", baseKey, targetType, rawFilter)
		}
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
		if err := store.ValidateSearchQuery(vals); err != nil {
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
	dOp, err := store.ParseDateOp(endRaw)
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
// ("status": "requested") to "active" per FHIR R4 §2.46.7.1, except for HL7 R5 Backport
// rest-hook Subscriptions which remain "requested" until the async handshake succeeds.
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
		} else if isBackportSubscription(m) {
			ch, _ := m["channel"].(map[string]any)
			if strings.EqualFold(strings.TrimSpace(stringField(ch, "type")), "rest-hook") {
				return rawJSON
			}
			m["status"] = "active"
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
// a newly created or updated resource and dispatches rest-hook and WebSocket notifications.
func (s *Server) dispatchSubscriptions(ctx context.Context, tenant, resType, resID string, resourceProto []byte, resVersion int, lastUpdated time.Time, idx *store.SearchIndexes) {
	action := "update"
	if resVersion <= 1 {
		action = "create"
	}
	s.dispatchSubscriptionsWithAction(ctx, tenant, resType, resID, resourceProto, resVersion, lastUpdated, idx, action)
}

// dispatchSubscriptionDelete evaluates active FHIR R4 Subscriptions in tenant against
// a soft-deleted resource (matching Subscription.criteria against the resource's prior version)
// and unbinds WebSocket streams when a Subscription itself is deleted.
func (s *Server) dispatchSubscriptionDelete(ctx context.Context, tenant, resType, resID string, resVersion int, lastUpdated time.Time) {
	if strings.EqualFold(resType, "Subscription") {
		s.cancelBackportHeartbeat(tenant, resID)
		s.syncSubscriptionWebSocketLifecycle(tenant, resID, nil)
		return
	}
	if s.store == nil || strings.EqualFold(resType, "AuditEvent") {
		return
	}
	sr, err := s.store.Search(ctx, store.SearchParams{
		TenantID: tenant,
		ResType:  "Subscription",
		Tokens:   map[string]string{"status": "active"},
		Count:    1,
	})
	if err != nil || sr == nil || len(sr.Matches) == 0 {
		return
	}
	var prevProto []byte
	var idx *store.SearchIndexes
	if history, _, hErr := s.store.ListResourceHistory(ctx, tenant, resType, resID, 5, 0); hErr == nil {
		for _, h := range history {
			if len(h.ResourceProto) > 0 {
				prevProto = h.ResourceProto
				if extractor, ok := s.extractors[resType]; ok {
					idx, _ = extractor(tenant, resID, prevProto)
				}
				break
			}
		}
	}
	if lastUpdated.IsZero() {
		lastUpdated = time.Now().UTC()
	}
	s.dispatchSubscriptionsWithAction(ctx, tenant, resType, resID, prevProto, resVersion, lastUpdated, idx, "delete")
}

func buildCloudEventNotification(
	tenant, subID, resType, resID string,
	resVersion int,
	lastUpdated time.Time,
	action, contentMode string,
	resMap map[string]any,
) map[string]any {
	ts := lastUpdated.UTC()
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	if action == "" {
		action = "update"
	}
	ce := map[string]any{
		"specversion":     "1.0",
		"id":              uuid.NewString(),
		"source":          fmt.Sprintf("/fhir/r4/%s/Subscription/%s", tenant, subID),
		"type":            "org.hl7.fhir.r4.subscription-notification",
		"subject":         fmt.Sprintf("%s/%s", resType, resID),
		"time":            ts.UTC().Format(time.RFC3339),
		"datacontenttype": "application/fhir+json",
		"fhiraction":      action,
	}
	switch contentMode {
	case "empty":
		// Omit data payload per empty content mode
	case "id-only":
		ce["data"] = map[string]any{
			"resourceType": resType,
			"id":           resID,
			"meta": map[string]any{
				"versionId":   strconv.Itoa(resVersion),
				"lastUpdated": ts.UTC().Format(time.RFC3339),
			},
		}
	default:
		if action == "delete" || resMap == nil {
			ce["data"] = map[string]any{
				"resourceType": resType,
				"id":           resID,
				"meta": map[string]any{
					"versionId":   strconv.Itoa(resVersion),
					"lastUpdated": ts.UTC().Format(time.RFC3339),
				},
			}
		} else {
			ce["data"] = resMap
		}
	}
	return ce
}

func (s *Server) dispatchSubscriptionsWithAction(ctx context.Context, tenant, resType, resID string, resourceProto []byte, resVersion int, lastUpdated time.Time, idx *store.SearchIndexes, action string) {
	if strings.EqualFold(resType, "Subscription") {
		s.syncSubscriptionWebSocketLifecycle(tenant, resID, resourceProto)
		s.syncBackportSubscriptionLifecycle(ctx, tenant, resID, resourceProto, resVersion, lastUpdated)
		return
	}
	if s.store == nil || strings.EqualFold(resType, "AuditEvent") {
		return
	}

	sr, err := s.store.Search(ctx, store.SearchParams{
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
		subMap, err := s.protoToFHIRMapWithMeta("Subscription", subRes.ResourceProto, subRes.ResVersion, subRes.LastUpdated)
		if err != nil {
			continue
		}

		if subscriptionIsExpired(subMap, now) {
			continue
		}

		criteria, _ := subMap["criteria"].(string)
		if !subscriptionMatchesSubscriptionMap(subMap, resType, resID, lastUpdated, idx) {
			continue
		}

		ev := s.recordSubscriptionEventWithAction(tenant, subRes.ResID, resType, resID, resVersion, lastUpdated, resourceProto, action)

		channel, _ := subMap["channel"].(map[string]any)
		if channel == nil {
			continue
		}
		chType, _ := channel["type"].(string)
		if strings.EqualFold(strings.TrimSpace(chType), "websocket") {
			status, _ := subMap["status"].(string)
			if status == "" {
				status = "active"
			}
			s.dispatchWebSocketSubscription(tenant, subRes.ResID, status, ev, resourceProto, idx)
			continue
		}

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
		mediaPayload := lowerPayload
		if semi := strings.IndexByte(mediaPayload, ';'); semi >= 0 {
			mediaPayload = strings.TrimSpace(mediaPayload[:semi])
		}
		contentMode, explicitContent := extractSubscriptionChannelContentMode(subMap)
		if !explicitContent && mediaPayload != "" {
			contentMode = "full-resource"
		}

		switch {
		case mediaPayload == "application/cloudevents+json":
			ce := buildCloudEventNotification(tenant, subRes.ResID, resType, resID, resVersion, lastUpdated, action, contentMode, ev.Resource)
			bodyBytes, _ = json.Marshal(ce)
			payloadType = "application/cloudevents+json; charset=utf-8"
		case mediaPayload == "application/cloudevents-batch+json":
			ce := buildCloudEventNotification(tenant, subRes.ResID, resType, resID, resVersion, lastUpdated, action, contentMode, ev.Resource)
			bodyBytes, _ = json.Marshal([]map[string]any{ce})
			payloadType = "application/cloudevents-batch+json; charset=utf-8"
		case isBackportSubscription(subMap) && (mediaPayload == "" || strings.Contains(mediaPayload, "json")):
			fhirBase := s.resolveTenantFHIRBaseURL(tenant)
			bundle := s.buildBackportNotificationBundle(fhirBase, tenant, subRes.ResID, criteria, "active", "event-notification", contentMode, []subscriptionEventRecord{ev})
			bodyBytes, _ = json.Marshal(bundle)
			if strings.TrimSpace(payloadType) == "" {
				payloadType = "application/fhir+json"
			}
		case mediaPayload == "empty" || (explicitContent && contentMode == "empty"):
			bodyBytes = nil
		case mediaPayload == "id-only" || (explicitContent && contentMode == "id-only") || (action == "delete" && strings.Contains(mediaPayload, "json")):
			bodyBytes, _ = json.Marshal(map[string]any{
				"resourceType": resType,
				"id":           resID,
				"meta": map[string]any{
					"versionId":   fmt.Sprintf("%d", resVersion),
					"lastUpdated": ev.Timestamp.UTC().Format(time.RFC3339),
				},
			})
			payloadType = "application/fhir+json; charset=utf-8"
		case strings.Contains(lowerPayload, "proto"):
			bodyBytes = resourceProto
		case strings.Contains(lowerPayload, "json"):
			if len(resourceProto) > 0 {
				if jb, err := s.protoToJSONWithMeta(resType, resourceProto, resVersion, lastUpdated); err == nil {
					bodyBytes = jb
				}
			}
		}

		subID := subRes.ResID
		// Dispatch webhook asynchronously via bounded worker semaphore so write latency is never blocked
		if s.subSem != nil {
			select {
			case s.subSem <- struct{}{}:
				sid, ep, pt, hdrs, body := subID, endpoint, payloadType, headers, bodyBytes
				started := s.startBackgroundWorker(func() {
					defer func() { <-s.subSem }()
					asyncCtx, cancel := s.deriveBackgroundContext(ctx)
					defer cancel()
					s.deliverRestHook(asyncCtx, tenant, sid, ep, pt, hdrs, body)
				})
				if !started {
					<-s.subSem
				}
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
			sid, ep, pt, hdrs, body := subID, endpoint, payloadType, headers, bodyBytes
			s.startBackgroundWorker(func() {
				asyncCtx, cancel := s.deriveBackgroundContext(ctx)
				defer cancel()
				s.deliverRestHook(asyncCtx, tenant, sid, ep, pt, hdrs, body)
			})
		}
	}
}

func subscriptionMatchesSubscriptionMap(subMap map[string]any, resType, resID string, lastUpdated time.Time, idx *store.SearchIndexes) bool {
	criteria, _ := subMap["criteria"].(string)
	trimmed := strings.TrimSpace(criteria)
	if trimmed == "" {
		return false
	}
	if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
		topicTarget, ok := supportedBackportTopics[trimmed]
		if !ok || !strings.EqualFold(topicTarget, resType) {
			return false
		}
		filters := extractBackportFilterCriteria(subMap)
		if len(filters) == 0 {
			return true
		}
		for _, f := range filters {
			norm := normalizeBackportFilterString(f)
			if !subscriptionMatchesResourceWithMeta(norm, resType, resID, lastUpdated, idx) {
				return false
			}
		}
		return true
	}
	filters := extractBackportFilterCriteria(subMap)
	if !subscriptionMatchesResourceWithMeta(trimmed, resType, resID, lastUpdated, idx) {
		return false
	}
	for _, f := range filters {
		norm := normalizeBackportFilterString(f)
		if !subscriptionMatchesResourceWithMeta(norm, resType, resID, lastUpdated, idx) {
			return false
		}
	}
	return true
}

func (s *Server) cancelBackportHeartbeat(tenant, subID string) {
	key := tenant + "/" + subID
	s.subBaseURLMu.Lock()
	if s.subHbCancel != nil {
		if fn, ok := s.subHbCancel[key]; ok {
			fn()
			delete(s.subHbCancel, key)
		}
	}
	s.subBaseURLMu.Unlock()
}

func (s *Server) syncBackportSubscriptionLifecycle(ctx context.Context, tenant, subID string, resourceProto []byte, resVersion int, lastUpdated time.Time) {
	if s.store == nil || len(resourceProto) == 0 {
		s.cancelBackportHeartbeat(tenant, subID)
		return
	}
	subMap, err := s.protoToFHIRMapWithMeta("Subscription", resourceProto, resVersion, lastUpdated)
	if err != nil || !isBackportSubscription(subMap) {
		return
	}
	status := strings.ToLower(strings.TrimSpace(stringField(subMap, "status")))
	ch, _ := subMap["channel"].(map[string]any)
	chType := strings.ToLower(strings.TrimSpace(stringField(ch, "type")))
	endpoint := strings.TrimSpace(stringField(ch, "endpoint"))
	if chType != "rest-hook" || endpoint == "" || subscriptionIsExpired(subMap, time.Now().UTC()) || (status != "requested" && status != "active") {
		s.cancelBackportHeartbeat(tenant, subID)
		return
	}
	if status != "requested" {
		return
	}

	s.cancelBackportHeartbeat(tenant, subID)
	payloadType := strings.TrimSpace(stringField(ch, "payload"))
	if payloadType == "" {
		payloadType = "application/fhir+json"
	}
	var headers []string
	if rawHeaders, ok := ch["header"].([]any); ok {
		for _, h := range rawHeaders {
			if hs, ok := h.(string); ok && strings.TrimSpace(hs) != "" {
				headers = append(headers, hs)
			}
		}
	}
	criteria := strings.TrimSpace(stringField(subMap, "criteria"))
	contentMode, explicitContent := extractSubscriptionChannelContentMode(subMap)
	if !explicitContent {
		contentMode = "full-resource"
	}
	hbPeriod := extractSubscriptionHeartbeatPeriod(subMap, 0)
	fhirBase := s.resolveTenantFHIRBaseURL(tenant)

	hbCtx, hbCancel := s.deriveBackgroundContext(ctx)
	key := tenant + "/" + subID
	s.subBaseURLMu.Lock()
	if s.subHbCancel == nil {
		s.subHbCancel = make(map[string]context.CancelFunc)
	}
	s.subHbCancel[key] = hbCancel
	s.subBaseURLMu.Unlock()

	started := s.startBackgroundWorker(func() {
		// Brief delay allows synchronous REST callers (e.g. HL7 Inferno NotificationDeliveryTest)
		// to receive the 201 Created ("requested") response and arm their webhook listener.
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-hbCtx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		hsBundle := s.buildBackportNotificationBundle(fhirBase, tenant, subID, criteria, "requested", "handshake", contentMode, nil)
		hsBytes, _ := json.Marshal(hsBundle)
		ok := s.deliverRestHookWithStatus(hbCtx, tenant, subID, endpoint, payloadType, headers, hsBytes, false)
		if !ok {
			_ = s.updateStoredSubscriptionStatus(hbCtx, tenant, subID, "error", "rest-hook handshake failed")
			return
		}
		_ = s.updateStoredSubscriptionStatus(hbCtx, tenant, subID, "active", "")

		if hbPeriod <= 0 {
			return
		}
		ticker := time.NewTicker(hbPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				rec, rErr := s.store.ReadResourceWithMeta(hbCtx, tenant, "Subscription", subID)
				if rErr != nil {
					return
				}
				curMap, mErr := s.protoToFHIRMapWithMeta("Subscription", rec.ResourceProto, rec.ResVersion, rec.LastUpdated)
				if mErr != nil || subscriptionIsExpired(curMap, time.Now().UTC()) || !strings.EqualFold(stringField(curMap, "status"), "active") {
					return
				}
				hbBundle := s.buildBackportNotificationBundle(fhirBase, tenant, subID, criteria, "active", "heartbeat", contentMode, nil)
				hbBytes, _ := json.Marshal(hbBundle)
				s.deliverRestHookWithStatus(hbCtx, tenant, subID, endpoint, payloadType, headers, hbBytes, false)
			}
		}
	})
	if !started {
		s.cancelBackportHeartbeat(tenant, subID)
	}
}

func (s *Server) updateStoredSubscriptionStatus(ctx context.Context, tenant, subID, newStatus, errMsg string) error {
	if s.store == nil {
		return nil
	}
	md, ok := s.protoRegistry["Subscription"]
	if !ok {
		return nil
	}
	rec, err := s.store.ReadResourceWithMeta(ctx, tenant, "Subscription", subID)
	if err != nil {
		return err
	}
	subMap, err := s.protoToFHIRMap("Subscription", rec.ResourceProto)
	if err != nil {
		return err
	}
	subMap["id"] = subID
	subMap["status"] = newStatus
	if errMsg != "" {
		subMap["error"] = errMsg
	} else {
		delete(subMap, "error")
	}
	rawJSON, err := json.Marshal(subMap)
	if err != nil {
		return err
	}
	normJSON, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, rawJSON)
	if err != nil {
		return err
	}
	msg := dynamicpb.NewMessage(md)
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(normJSON, msg); err != nil {
		return err
	}
	setProtoResourceID(msg, subID)
	protoBytes, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	var idx *store.SearchIndexes
	if extractor, hasExt := s.extractors["Subscription"]; hasExt {
		idx, _ = extractor(tenant, subID, protoBytes)
	}
	_, err = s.store.WriteResourceWithMeta(ctx, store.ResourceInput{
		TenantID:      tenant,
		ResType:       "Subscription",
		ResID:         subID,
		ResourceProto: protoBytes,
		SearchIndexes: idx,
	})
	return err
}

func (s *Server) buildBackportNotificationBundle(
	fhirBase, tenant, subID, topicCanonical, status, notificationType, contentMode string,
	matched []subscriptionEventRecord,
) map[string]any {
	fhirBase = strings.TrimRight(strings.TrimSpace(fhirBase), "/")
	if fhirBase == "" {
		fhirBase = s.resolveTenantFHIRBaseURL(tenant)
	}
	stats := s.getSubscriptionDeliveryStats(tenant, subID)
	totalEvents := stats.eventsDelivered
	if n := len(stats.events); n > 0 && stats.events[n-1].EventNumber > totalEvents {
		totalEvents = stats.events[n-1].EventNumber
	}
	if notificationType == "handshake" {
		totalEvents = 0
	}

	statusParams := []map[string]any{
		{
			"name": "subscription",
			"valueReference": map[string]any{
				"reference": fhirBase + "/Subscription/" + subID,
			},
		},
	}
	if topicCanonical != "" && (strings.HasPrefix(topicCanonical, "http://") || strings.HasPrefix(topicCanonical, "https://")) && contentMode != "empty" {
		statusParams = append(statusParams, map[string]any{
			"name":           "topic",
			"valueCanonical": topicCanonical,
		})
	}
	statusParams = append(statusParams,
		map[string]any{
			"name":      "status",
			"valueCode": status,
		},
		map[string]any{
			"name":      "type",
			"valueCode": notificationType,
		},
		map[string]any{
			"name":        "events-since-subscription-start",
			"valueString": strconv.FormatInt(totalEvents, 10),
		},
	)

	if notificationType == "event-notification" {
		for _, ev := range matched {
			parts := []map[string]any{
				{
					"name":        "event-number",
					"valueString": strconv.FormatInt(ev.EventNumber, 10),
				},
				{
					"name":         "timestamp",
					"valueInstant": ev.Timestamp.UTC().Format(time.RFC3339),
				},
			}
			if contentMode != "empty" {
				parts = append(parts, map[string]any{
					"name": "focus",
					"valueReference": map[string]any{
						"reference": fhirBase + "/" + ev.ResType + "/" + ev.ResID,
					},
				})
			}
			statusParams = append(statusParams, map[string]any{
				"name": "notification-event",
				"part": parts,
			})
		}
	}

	statusID := uuid.NewString()
	statusRes := map[string]any{
		"resourceType": "Parameters",
		"id":           statusID,
		"meta": map[string]any{
			"profile": []string{backportSubscriptionStatusProfileURL},
		},
		"parameter": statusParams,
	}

	entries := []map[string]any{
		{
			"fullUrl":  "urn:uuid:" + statusID,
			"resource": statusRes,
			"request": map[string]any{
				"method": "GET",
				"url":    fhirBase + "/Subscription/" + subID + "/$status",
			},
			"response": map[string]any{
				"status": "200",
			},
		},
	}

	if notificationType == "event-notification" && contentMode != "empty" {
		for _, ev := range matched {
			reqMethod := "PUT"
			reqURL := ev.ResType + "/" + ev.ResID
			respStatus := "200"
			switch ev.Action {
			case "create":
				reqMethod = "POST"
				reqURL = ev.ResType
				respStatus = "201"
			case "delete":
				reqMethod = "DELETE"
				respStatus = "204"
			}
			entry := map[string]any{
				"fullUrl": fhirBase + "/" + ev.ResType + "/" + ev.ResID,
				"request": map[string]any{
					"method": reqMethod,
					"url":    reqURL,
				},
				"response": map[string]any{
					"status": respStatus,
				},
			}
			if contentMode == "full-resource" && ev.Action != "delete" && ev.Resource != nil {
				entry["resource"] = ev.Resource
			}
			entries = append(entries, entry)
		}
	}

	return map[string]any{
		"resourceType": "Bundle",
		"id":           uuid.NewString(),
		"meta": map[string]any{
			"profile": []string{backportNotificationBundleProfileURL},
		},
		"type":      "history",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"entry":     entries,
	}
}

func subscriptionMatchesResourceWithMeta(criteria, resType, resID string, lastUpdated time.Time, idx *store.SearchIndexes) bool {
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
	sp := store.BuildSearchParamsFromQuery("", resType, vals)

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
		idx = &store.SearchIndexes{}
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
		dateFilters = make(map[string][]store.DateOp, len(sp.Dates))
		for k, v := range sp.Dates {
			dateFilters[k] = []store.DateOp{v}
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
		quantityFilters = make(map[string][]store.QuantityOp, len(sp.Quantities))
		for k, v := range sp.Quantities {
			quantityFilters[k] = []store.QuantityOp{v}
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

func subscriptionIdxHasParam(idx *store.SearchIndexes, spName string) bool {
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

func matchSubscriptionToken(tokens []store.SpidxToken, baseName, want string) bool {
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

func matchSubscriptionDateOp(spLow, spHigh time.Time, op store.DateOp) bool {
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
			return spLow.Before(low) || !spLow.Before(high) || spHigh.After(high)
		}
		return spLow.Before(low) || spHigh.After(low)
	case "ap":
		approxLow := low.Add(-24 * time.Hour)
		approxHigh := high.Add(24 * time.Hour)
		return !spLow.After(approxHigh) && !spHigh.Before(approxLow)
	default: // "eq"
		if hasInterval {
			return !spLow.Before(low) && spLow.Before(high) && !spHigh.After(high)
		}
		return !spLow.Before(low) && !spHigh.After(low)
	}
}

func matchSubscriptionQuantityOp(qt store.SpidxQuantity, op store.QuantityOp) bool {
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

func matchSubscriptionCompositeTokenQuantity(idx *store.SearchIndexes, cp store.CompositeTokenQuantityParam) bool {
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
	_ = s.deliverRestHookWithStatus(ctx, tenant, subID, endpoint, payloadType, headers, bodyBytes, true)
}

func (s *Server) deliverRestHookWithStatus(ctx context.Context, tenant, subID, endpoint, payloadType string, headers []string, bodyBytes []byte, countEvent bool) bool {
	if err := ValidateSubscriptionEndpoint(endpoint, s.disallowPrivateWebhooks); err != nil {
		s.recordSubscriptionDelivery(tenant, subID, false, err.Error())
		if s.metrics != nil {
			s.metrics.subscriptionDeliveryErrorsTotal.Add(1)
		}
		return false
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
	defer transport.CloseIdleConnections()

	const maxAttempts = 2
	var lastErr error
	var lastStatus int

	for attempt := range maxAttempts {
		if ctx.Err() != nil {
			return false
		}
		if attempt > 0 {
			timer := time.NewTimer(50 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return false
			case <-timer.C:
			}
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
			if countEvent {
				s.recordSubscriptionDelivery(tenant, subID, true, "")
			}
			if s.metrics != nil {
				s.metrics.subscriptionsDeliveredTotal.Add(1)
			}
			return true
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
	return false
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
			if raw, err := s.decodeOperationBodyMap(r, body, "Parameters"); err == nil {
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
		if errors.Is(err, store.ErrResourceDeleted) {
			writeOperationOutcome(w, http.StatusGone, "deleted",
				fmt.Sprintf("Subscription/%s has been deleted", subID))
			return
		}
		if err != nil {
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to read Subscription")
			return
		}
		s.recordTenantFHIRBaseURL(r, tenant)
		subMap, mapErr := s.protoToFHIRMapWithMeta("Subscription", rec.ResourceProto, rec.ResVersion, rec.LastUpdated)
		wantsProto, _ := wantsProtoResponse(r)
		if mapErr == nil && isBackportSubscription(subMap) && !wantsProto {
			statusParams := s.buildBackportQueryStatusParameters(tenant, subID, subMap, now)
			statusID, _ := statusParams["id"].(string)
			bundle := map[string]any{
				"resourceType": "Bundle",
				"id":           uuid.NewString(),
				"type":         "searchset",
				"total":        1,
				"entry": []map[string]any{
					{
						"fullUrl":  "urn:uuid:" + statusID,
						"resource": statusParams,
						"search": map[string]any{
							"mode": "match",
						},
					},
				},
			}
			s.writeOperationResponse(w, r, http.StatusOK, "Bundle", bundle)
			return
		}
		statusParams, err := s.buildSubscriptionStatusParameters(tenant, subID, rec.ResourceProto, rec.ResVersion, rec.LastUpdated, now)
		if err != nil {
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to format Subscription status")
			return
		}
		s.writeOperationResponse(w, r, http.StatusOK, "Parameters", statusParams)
		return
	}

	sr, err := s.store.Search(r.Context(), store.SearchParams{
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
	s.writeOperationResponse(w, r, http.StatusOK, "Bundle", bundle)
}

func (s *Server) buildBackportQueryStatusParameters(tenant, subID string, subMap map[string]any, now time.Time) map[string]any {
	fhirBase := s.resolveTenantFHIRBaseURL(tenant)
	status, _ := subMap["status"].(string)
	if status == "" {
		status = "active"
	}
	if subscriptionIsExpired(subMap, now) {
		status = "off"
	}
	criteria := strings.TrimSpace(stringField(subMap, "criteria"))
	stats := s.getSubscriptionDeliveryStats(tenant, subID)
	totalEvents := stats.eventsDelivered
	if n := len(stats.events); n > 0 && stats.events[n-1].EventNumber > totalEvents {
		totalEvents = stats.events[n-1].EventNumber
	}

	params := []map[string]any{
		{
			"name": "subscription",
			"valueReference": map[string]any{
				"reference": fhirBase + "/Subscription/" + subID,
			},
		},
	}
	if criteria != "" && (strings.HasPrefix(criteria, "http://") || strings.HasPrefix(criteria, "https://")) {
		params = append(params, map[string]any{
			"name":           "topic",
			"valueCanonical": criteria,
		})
	}
	params = append(params,
		map[string]any{
			"name":      "status",
			"valueCode": status,
		},
		map[string]any{
			"name":      "type",
			"valueCode": "query-status",
		},
		map[string]any{
			"name":        "events-since-subscription-start",
			"valueString": strconv.FormatInt(totalEvents, 10),
		},
	)
	if effectiveErr := strings.TrimSpace(stats.lastError); effectiveErr != "" {
		params = append(params, map[string]any{
			"name": "error",
			"valueCodeableConcept": map[string]any{
				"text": effectiveErr,
			},
		})
	}
	statusID := uuid.NewString()
	return map[string]any{
		"resourceType": "Parameters",
		"id":           statusID,
		"meta": map[string]any{
			"profile": []string{backportSubscriptionStatusProfileURL},
		},
		"parameter": params,
	}
}

func (s *Server) buildSubscriptionStatusParameters(
	tenant, subID string,
	resourceProto []byte,
	resVersion int,
	lastUpdated time.Time,
	now time.Time,
) (map[string]any, error) {
	subMap, err := s.protoToFHIRMapWithMeta("Subscription", resourceProto, resVersion, lastUpdated)
	if err != nil {
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

// handleSubscriptionEvents handles GET/POST /fhir/r4/{tenant}/Subscription/{id}/$events
// and GET/POST /fhir/r4/{tenant}/Subscription/$events per the HL7 FHIR Subscriptions
// R5 Backport IG, replaying missed notification events as a history Bundle whose first
// entry is the R4 Backport SubscriptionStatus Parameters resource ("type": "event-notification").
func (s *Server) handleSubscriptionEvents(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	if resType := r.PathValue("resourceType"); resType != "" && resType != "Subscription" {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("$events is only supported on Subscription resources (got %s)", resType))
		return
	}
	if secCtx, hasSecCtx := auth.FromContext(r.Context()); hasSecCtx {
		if !auth.Allows(secCtx, "Subscription", auth.ActionRead) && !auth.Allows(secCtx, "Subscription", auth.ActionSearch) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden", "insufficient scope to read Subscription events")
			return
		}
	}

	subID := strings.TrimSpace(r.PathValue("id"))
	if subID == "" {
		subID = strings.TrimSpace(r.URL.Query().Get("id"))
	}
	sinceStr := strings.TrimSpace(r.URL.Query().Get("eventsSinceNumber"))
	untilStr := strings.TrimSpace(r.URL.Query().Get("eventsUntilNumber"))
	contentMode := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("content")))

	if r.Method == http.MethodPost && r.Body != nil && r.ContentLength != 0 {
		if body, err := io.ReadAll(io.LimitReader(r.Body, 1*1024*1024)); err == nil && len(bytes.TrimSpace(body)) > 0 {
			if raw, err := s.decodeOperationBodyMap(r, body, "Parameters"); err == nil {
				if paramsArr, ok := raw["parameter"].([]any); ok {
					for _, p := range paramsArr {
						pm, _ := p.(map[string]any)
						name, _ := pm["name"].(string)
						switch name {
						case "id", "subscription":
							if subID == "" {
								if sVal, ok := pm["valueString"].(string); ok && strings.TrimSpace(sVal) != "" {
									subID = cleanID(sVal)
								} else if cVal, ok := pm["valueId"].(string); ok && strings.TrimSpace(cVal) != "" {
									subID = cleanID(cVal)
								} else if refMap, ok := pm["valueReference"].(map[string]any); ok {
									if refStr, ok := refMap["reference"].(string); ok && strings.TrimSpace(refStr) != "" {
										subID = cleanID(refStr)
									}
								}
							}
						case "eventsSinceNumber":
							if sinceStr == "" {
								if sVal, ok := pm["valueString"].(string); ok {
									sinceStr = strings.TrimSpace(sVal)
								} else if fVal, ok := pm["valueInteger"].(float64); ok {
									sinceStr = strconv.FormatInt(int64(fVal), 10)
								} else if fVal, ok := pm["valueInteger64"].(string); ok {
									sinceStr = strings.TrimSpace(fVal)
								}
							}
						case "eventsUntilNumber":
							if untilStr == "" {
								if sVal, ok := pm["valueString"].(string); ok {
									untilStr = strings.TrimSpace(sVal)
								} else if fVal, ok := pm["valueInteger"].(float64); ok {
									untilStr = strconv.FormatInt(int64(fVal), 10)
								} else if fVal, ok := pm["valueInteger64"].(string); ok {
									untilStr = strings.TrimSpace(fVal)
								}
							}
						case "content":
							if contentMode == "" {
								if cVal, ok := pm["valueCode"].(string); ok {
									contentMode = strings.ToLower(strings.TrimSpace(cVal))
								} else if sVal, ok := pm["valueString"].(string); ok {
									contentMode = strings.ToLower(strings.TrimSpace(sVal))
								}
							}
						}
					}
				}
			}
		}
	}

	if subID == "" {
		writeOperationOutcome(w, http.StatusBadRequest, "required",
			"Subscription/$events requires a Subscription {id} path parameter or subscription parameter")
		return
	}
	if contentMode != "" && contentMode != "empty" && contentMode != "id-only" && contentMode != "full-resource" {
		writeOperationOutcome(w, http.StatusBadRequest, "value",
			fmt.Sprintf("unsupported Subscription/$events content mode %q (expected empty, id-only, or full-resource)", contentMode))
		return
	}

	var sinceNum, untilNum int64
	hasSince := false
	hasUntil := false
	if sinceStr != "" {
		v, err := strconv.ParseInt(sinceStr, 10, 64)
		if err != nil || v < 0 {
			writeOperationOutcome(w, http.StatusBadRequest, "value", "invalid eventsSinceNumber parameter")
			return
		}
		sinceNum = v
		hasSince = true
	}
	if untilStr != "" {
		v, err := strconv.ParseInt(untilStr, 10, 64)
		if err != nil || v < 0 {
			writeOperationOutcome(w, http.StatusBadRequest, "value", "invalid eventsUntilNumber parameter")
			return
		}
		untilNum = v
		hasUntil = true
	}

	if s.store == nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "operational store is not configured")
		return
	}

	rec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, "Subscription", subID)
	if errors.Is(err, sql.ErrNoRows) {
		writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("Subscription/%s not found", subID))
		return
	}
	if errors.Is(err, store.ErrResourceDeleted) {
		writeOperationOutcome(w, http.StatusGone, "deleted", fmt.Sprintf("Subscription/%s has been deleted", subID))
		return
	}
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to read Subscription")
		return
	}

	subMap, err := s.protoToFHIRMapWithMeta("Subscription", rec.ResourceProto, rec.ResVersion, rec.LastUpdated)
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to decode Subscription")
		return
	}

	now := time.Now().UTC()
	status, _ := subMap["status"].(string)
	if subscriptionIsExpired(subMap, now) {
		status = "off"
	}
	if contentMode == "" {
		if cm, explicit := extractSubscriptionChannelContentMode(subMap); explicit {
			contentMode = cm
		} else {
			contentMode = "full-resource"
		}
	}

	stats := s.getSubscriptionDeliveryStats(tenant, subID)
	matched := make([]subscriptionEventRecord, 0, len(stats.events))
	for _, ev := range stats.events {
		if hasSince && ev.EventNumber < sinceNum {
			continue
		}
		if hasUntil && ev.EventNumber > untilNum {
			continue
		}
		matched = append(matched, ev)
	}

	bundle := s.buildSubscriptionNotificationBundle(tenant, subID, status, "event-notification", contentMode, matched)
	s.writeOperationResponse(w, r, http.StatusOK, "Bundle", bundle)
}
