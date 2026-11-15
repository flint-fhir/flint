// Package txclient implements a cached, pure-Go HTTP FHIR R4 terminology client
// for federating ValueSet and CodeSystem operations ($expand, $validate-code,
// $lookup, $subsumes) to an upstream HL7 FHIR R4 terminology server.
package txclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultTimeout is the default per-request HTTP timeout when calling the upstream terminology server.
	DefaultTimeout = 5 * time.Second
	// DefaultCacheTTL is the default time-to-live for cached terminology responses.
	DefaultCacheTTL = 15 * time.Minute
	// DefaultCacheMaxEntries is the default maximum number of responses retained in the LRU cache.
	DefaultCacheMaxEntries = 4096
	// maxResponseBytes caps upstream response payloads to 16 MiB.
	maxResponseBytes = 16 * 1024 * 1024
)

// Config configures a federated FHIR R4 terminology Client.
type Config struct {
	// BaseURL is the root FHIR R4 endpoint of the upstream terminology server (e.g. "https://tx.fhir.org/r4").
	BaseURL string
	// Timeout bounds each outbound HTTP terminology request. Defaults to DefaultTimeout (5s) when <= 0.
	Timeout time.Duration
	// CacheTTL sets the TTL for cached responses. Zero uses DefaultCacheTTL (15m); negative disables caching.
	CacheTTL time.Duration
	// CacheMaxEntries bounds the in-memory LRU cache capacity. Zero uses DefaultCacheMaxEntries (4096); negative disables caching.
	CacheMaxEntries int
	// AuthHeader sets an optional authentication header on outbound requests.
	// Values containing ":" (e.g. "X-Api-Key: secret") are sent as that header name/value;
	// bare values (e.g. "Bearer <jwt>") are sent in the "Authorization" header.
	AuthHeader string
	// FailOpen controls fallback behavior when the upstream server is unreachable or returns 5xx.
	// Nil defaults to true (fail-open).
	FailOpen *bool
	// HTTPClient overrides the underlying *http.Client (primarily for TLS customization or tests).
	HTTPClient *http.Client
	// NowFunc overrides the clock used by the TTL cache (for deterministic tests).
	NowFunc func() time.Time
}

// BoolPtr returns a pointer to b for configuring Config.FailOpen.
func BoolPtr(b bool) *bool {
	return &b
}

// UpstreamError represents an HTTP or transport failure from the upstream terminology server.
type UpstreamError struct {
	StatusCode int
	Outcome    map[string]any
	Message    string
	Err        error
}

func (e *UpstreamError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		if e.StatusCode > 0 {
			return fmt.Sprintf("upstream terminology server HTTP %d: %s", e.StatusCode, e.Message)
		}
		return fmt.Sprintf("upstream terminology server error: %s", e.Message)
	}
	if e.Err != nil {
		return fmt.Sprintf("upstream terminology server error: %v", e.Err)
	}
	return fmt.Sprintf("upstream terminology server HTTP %d", e.StatusCode)
}

func (e *UpstreamError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// IsNotFound reports whether the upstream server returned HTTP 404.
func (e *UpstreamError) IsNotFound() bool {
	return e != nil && e.StatusCode == http.StatusNotFound
}

// IsClientError reports whether the upstream server returned a 4xx client error (excluding 429 Too Many Requests).
func (e *UpstreamError) IsClientError() bool {
	return e != nil && e.StatusCode >= 400 && e.StatusCode < 500 && e.StatusCode != http.StatusTooManyRequests
}

// IsTransient reports whether the failure is a network/timeout error, HTTP 429, or HTTP 5xx.
func (e *UpstreamError) IsTransient() bool {
	if e == nil {
		return false
	}
	return e.StatusCode == 0 || e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// AsUpstreamError extracts an *UpstreamError from err if present.
func AsUpstreamError(err error) (*UpstreamError, bool) {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue, true
	}
	return nil, false
}

// Client is a concurrency-safe federated HL7 FHIR R4 terminology client with bounded LRU+TTL caching.
type Client struct {
	baseURL    string
	timeout    time.Duration
	headerName string
	headerVal  string
	failOpen   bool
	httpClient *http.Client
	cache      *lruCache
	inflight   inFlightGroup
}

// New creates and validates a federated FHIR R4 terminology Client.
func New(cfg Config) (*Client, error) {
	rawURL := strings.TrimSpace(cfg.BaseURL)
	if rawURL == "" {
		return nil, fmt.Errorf("txclient: BaseURL is required")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("txclient: BaseURL %q must be a valid http or https URL", rawURL)
	}
	baseURL := strings.TrimRight(rawURL, "/")

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	ttl := cfg.CacheTTL
	if ttl == 0 {
		ttl = DefaultCacheTTL
	}
	maxEntries := cfg.CacheMaxEntries
	if maxEntries == 0 {
		maxEntries = DefaultCacheMaxEntries
	}

	failOpen := true
	if cfg.FailOpen != nil {
		failOpen = *cfg.FailOpen
	}

	var hName, hVal string
	if rawAuth := strings.TrimSpace(cfg.AuthHeader); rawAuth != "" {
		if strings.ContainsAny(rawAuth, "\r\n") {
			return nil, fmt.Errorf("txclient: AuthHeader must not contain CR/LF characters")
		}
		if idx := strings.Index(rawAuth, ":"); idx > 0 && !strings.HasPrefix(strings.ToLower(rawAuth), "bearer ") && !strings.HasPrefix(strings.ToLower(rawAuth), "basic ") {
			hName = strings.TrimSpace(rawAuth[:idx])
			hVal = strings.TrimSpace(rawAuth[idx+1:])
		} else {
			hName = "Authorization"
			hVal = rawAuth
		}
	}

	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: timeout}
	}

	var cache *lruCache
	if ttl > 0 && maxEntries > 0 {
		cache = newLRUCache(maxEntries, ttl, cfg.NowFunc)
	}

	return &Client{
		baseURL:    baseURL,
		timeout:    timeout,
		headerName: hName,
		headerVal:  hVal,
		failOpen:   failOpen,
		httpClient: hc,
		cache:      cache,
	}, nil
}

// BaseURL returns the normalized upstream FHIR R4 terminology server base URL.
func (c *Client) BaseURL() string {
	if c == nil {
		return ""
	}
	return c.baseURL
}

// FailOpen reports whether transient upstream errors should degrade gracefully rather than failing closed.
func (c *Client) FailOpen() bool {
	if c == nil {
		return true
	}
	return c.failOpen
}

// Timeout returns the configured per-request timeout.
func (c *Client) Timeout() time.Duration {
	if c == nil {
		return DefaultTimeout
	}
	return c.timeout
}

// CacheStats returns the current entry count, cache hit count, and cache miss count.
func (c *Client) CacheStats() (entries int, hits uint64, misses uint64) {
	if c == nil || c.cache == nil {
		return 0, 0, 0
	}
	return c.cache.len(), c.cache.hits.Load(), c.cache.misses.Load()
}

// PurgeCache clears all cached terminology responses.
func (c *Client) PurgeCache() {
	if c != nil && c.cache != nil {
		c.cache.purge()
	}
}

var expandAllowedParams = []string{
	"url",
	"valueSetVersion",
	"context",
	"contextDirection",
	"filter",
	"date",
	"offset",
	"count",
	"includeDesignations",
	"designation",
	"includeDefinition",
	"activeOnly",
	"excludeNested",
	"excludeNotForUI",
	"excludePostCoordinated",
	"displayLanguage",
	"exclude-system",
	"system-version",
	"check-system-version",
	"force-system-version",
}

// ExpandValueSet invokes ValueSet/$expand on the upstream terminology server.
func (c *Client) ExpandValueSet(ctx context.Context, params map[string]string, inlineVS map[string]any) (map[string]any, error) {
	q := pickQueryParams(params, expandAllowedParams)
	if inlineVS != nil {
		bodyParams := buildParametersResource(q, "valueSet", inlineVS, expandBooleanParams, expandIntegerParams)
		return c.executeCachedPOST(ctx, "ValueSet/$expand", bodyParams, "ValueSet")
	}
	return c.executeCachedGET(ctx, "ValueSet/$expand", q, "ValueSet")
}

var validateVSAllowedParams = []string{
	"url",
	"context",
	"valueSetVersion",
	"code",
	"system",
	"systemVersion",
	"display",
	"date",
	"abstract",
	"displayLanguage",
}

// ValidateValueSetCode invokes ValueSet/$validate-code on the upstream terminology server.
func (c *Client) ValidateValueSetCode(ctx context.Context, params map[string]string, inlineVS map[string]any) (map[string]any, error) {
	q := pickQueryParams(params, validateVSAllowedParams)
	if inlineVS != nil {
		bodyParams := buildParametersResource(q, "valueSet", inlineVS, validateBooleanParams, nil)
		return c.executeCachedPOST(ctx, "ValueSet/$validate-code", bodyParams, "Parameters")
	}
	return c.executeCachedGET(ctx, "ValueSet/$validate-code", q, "Parameters")
}

var lookupAllowedParams = []string{
	"code",
	"system",
	"version",
	"date",
	"displayLanguage",
	"property",
}

// LookupCode invokes CodeSystem/$lookup on the upstream terminology server.
func (c *Client) LookupCode(ctx context.Context, params map[string]string) (map[string]any, error) {
	q := pickQueryParams(params, lookupAllowedParams)
	return c.executeCachedGET(ctx, "CodeSystem/$lookup", q, "Parameters")
}

var validateCSAllowedParams = []string{
	"url",
	"code",
	"version",
	"display",
	"date",
	"abstract",
	"displayLanguage",
}

// ValidateCodeSystemCode invokes CodeSystem/$validate-code on the upstream terminology server.
func (c *Client) ValidateCodeSystemCode(ctx context.Context, params map[string]string, inlineCS map[string]any) (map[string]any, error) {
	normalized := make(map[string]string, len(params))
	for k, v := range params {
		normalized[k] = v
	}
	if strings.TrimSpace(normalized["url"]) == "" && strings.TrimSpace(normalized["system"]) != "" {
		normalized["url"] = strings.TrimSpace(normalized["system"])
	}
	if strings.TrimSpace(normalized["version"]) == "" && strings.TrimSpace(normalized["systemVersion"]) != "" {
		normalized["version"] = strings.TrimSpace(normalized["systemVersion"])
	}
	q := pickQueryParams(normalized, validateCSAllowedParams)
	if inlineCS != nil {
		bodyParams := buildParametersResource(q, "codeSystem", inlineCS, validateBooleanParams, nil)
		return c.executeCachedPOST(ctx, "CodeSystem/$validate-code", bodyParams, "Parameters")
	}
	return c.executeCachedGET(ctx, "CodeSystem/$validate-code", q, "Parameters")
}

var subsumesAllowedParams = []string{
	"codeA",
	"codeB",
	"system",
	"version",
}

// Subsumes invokes CodeSystem/$subsumes on the upstream terminology server.
func (c *Client) Subsumes(ctx context.Context, params map[string]string, inlineCS map[string]any) (map[string]any, error) {
	normalized := make(map[string]string, len(params))
	for k, v := range params {
		normalized[k] = v
	}
	if strings.TrimSpace(normalized["system"]) == "" && strings.TrimSpace(normalized["url"]) != "" {
		normalized["system"] = strings.TrimSpace(normalized["url"])
	}
	q := pickQueryParams(normalized, subsumesAllowedParams)
	if inlineCS != nil {
		bodyParams := buildParametersResource(q, "codeSystem", inlineCS, nil, nil)
		return c.executeCachedPOST(ctx, "CodeSystem/$subsumes", bodyParams, "Parameters")
	}
	return c.executeCachedGET(ctx, "CodeSystem/$subsumes", q, "Parameters")
}

// ParseValidateCodeResult extracts result, display, system, and message from a FHIR R4 $validate-code Parameters map.
func ParseValidateCodeResult(paramsMap map[string]any) (valid bool, display string, system string, message string, ok bool) {
	rawList, exists := paramsMap["parameter"].([]any)
	if !exists {
		if typedList, okTyped := paramsMap["parameter"].([]map[string]any); okTyped {
			rawList = make([]any, len(typedList))
			for i := range typedList {
				rawList[i] = typedList[i]
			}
		} else {
			return false, "", "", "", false
		}
	}
	hasResult := false
	for _, item := range rawList {
		pm, isMap := item.(map[string]any)
		if !isMap {
			continue
		}
		name, _ := pm["name"].(string)
		switch name {
		case "result":
			if b, isBool := pm["valueBoolean"].(bool); isBool {
				valid = b
				hasResult = true
			}
		case "display":
			if s, isStr := pm["valueString"].(string); isStr {
				display = s
			}
		case "system":
			if s, isStr := pm["valueUri"].(string); isStr {
				system = s
			} else if s, isStr := pm["valueString"].(string); isStr {
				system = s
			}
		case "message":
			if s, isStr := pm["valueString"].(string); isStr {
				message = s
			}
		}
	}
	return valid, display, system, message, hasResult
}

// ParseSubsumesOutcome extracts the "outcome" code ("equivalent", "subsumes", "subsumed-by", "not-subsumed")
// from a FHIR R4 CodeSystem/$subsumes Parameters response.
func ParseSubsumesOutcome(paramsMap map[string]any) (string, bool) {
	rawList, exists := paramsMap["parameter"].([]any)
	if !exists {
		return "", false
	}
	for _, item := range rawList {
		pm, isMap := item.(map[string]any)
		if !isMap {
			continue
		}
		name, _ := pm["name"].(string)
		if name == "outcome" {
			if code, ok := pm["valueCode"].(string); ok && code != "" {
				return code, true
			}
			if code, ok := pm["valueString"].(string); ok && code != "" {
				return code, true
			}
		}
	}
	return "", false
}

var expandBooleanParams = map[string]bool{
	"includeDesignations":    true,
	"includeDefinition":      true,
	"activeOnly":             true,
	"excludeNested":          true,
	"excludeNotForUI":        true,
	"excludePostCoordinated": true,
}

var expandIntegerParams = map[string]bool{
	"offset": true,
	"count":  true,
}

var validateBooleanParams = map[string]bool{
	"abstract": true,
}

func pickQueryParams(in map[string]string, allowed []string) url.Values {
	q := url.Values{}
	if len(in) == 0 {
		return q
	}
	for _, key := range allowed {
		if val := strings.TrimSpace(in[key]); val != "" {
			q.Set(key, val)
		}
	}
	if key := "url"; q.Get(key) != "" {
		rawURL := q.Get(key)
		if pipe := strings.IndexByte(rawURL, '|'); pipe != -1 {
			ver := strings.TrimSpace(rawURL[pipe+1:])
			base := strings.TrimSpace(rawURL[:pipe])
			q.Set("url", base)
			if ver != "" {
				for _, ak := range allowed {
					if ak == "valueSetVersion" && q.Get("valueSetVersion") == "" {
						q.Set("valueSetVersion", ver)
					}
					if ak == "version" && q.Get("version") == "" {
						q.Set("version", ver)
					}
				}
			}
		}
	}
	return q
}

func buildParametersResource(q url.Values, resParamName string, inlineRes map[string]any, boolKeys, intKeys map[string]bool) map[string]any {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var params []map[string]any
	if inlineRes != nil {
		params = append(params, map[string]any{
			"name":     resParamName,
			"resource": inlineRes,
		})
	}
	for _, k := range keys {
		val := q.Get(k)
		if val == "" {
			continue
		}
		if boolKeys[k] {
			if b, err := strconv.ParseBool(val); err == nil {
				params = append(params, map[string]any{"name": k, "valueBoolean": b})
				continue
			}
		}
		if intKeys[k] {
			if n, err := strconv.Atoi(val); err == nil {
				params = append(params, map[string]any{"name": k, "valueInteger": n})
				continue
			}
		}
		switch k {
		case "url", "system", "context":
			params = append(params, map[string]any{"name": k, "valueUri": val})
		case "code", "codeA", "codeB", "displayLanguage":
			params = append(params, map[string]any{"name": k, "valueCode": val})
		default:
			params = append(params, map[string]any{"name": k, "valueString": val})
		}
	}
	return map[string]any{
		"resourceType": "Parameters",
		"parameter":    params,
	}
}

func (c *Client) executeCachedGET(ctx context.Context, opPath string, q url.Values, expectedResType string) (map[string]any, error) {
	encoded := q.Encode()
	endpoint := c.baseURL + "/" + strings.TrimLeft(opPath, "/")
	if encoded != "" {
		endpoint += "?" + encoded
	}
	cacheKey := "GET:" + endpoint
	if cached, ok := c.cache.get(cacheKey); ok {
		return cached, nil
	}
	return c.inflight.do(cacheKey, func() (map[string]any, error) {
		if cached, ok := c.cache.get(cacheKey); ok {
			return cached, nil
		}
		res, err := c.doFHIRRequest(ctx, http.MethodGet, endpoint, nil, expectedResType)
		if err != nil {
			return nil, err
		}
		c.cache.put(cacheKey, res)
		return res, nil
	})
}

func (c *Client) executeCachedPOST(ctx context.Context, opPath string, bodyMap map[string]any, expectedResType string) (map[string]any, error) {
	rawBody, err := json.Marshal(bodyMap)
	if err != nil {
		return nil, fmt.Errorf("txclient: marshal %s request: %w", opPath, err)
	}
	sum := sha256.Sum256(rawBody)
	endpoint := c.baseURL + "/" + strings.TrimLeft(opPath, "/")
	cacheKey := "POST:" + endpoint + ":" + hex.EncodeToString(sum[:])
	if cached, ok := c.cache.get(cacheKey); ok {
		return cached, nil
	}
	return c.inflight.do(cacheKey, func() (map[string]any, error) {
		if cached, ok := c.cache.get(cacheKey); ok {
			return cached, nil
		}
		res, err := c.doFHIRRequest(ctx, http.MethodPost, endpoint, rawBody, expectedResType)
		if err != nil {
			return nil, err
		}
		c.cache.put(cacheKey, res)
		return res, nil
	})
}

func (c *Client) doFHIRRequest(ctx context.Context, method, endpoint string, body []byte, expectedResType string) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, endpoint, bodyReader)
	if err != nil {
		return nil, &UpstreamError{Message: err.Error(), Err: err}
	}
	req.Header.Set("Accept", "application/fhir+json, application/json;q=0.9")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/fhir+json; charset=utf-8")
	}
	if c.headerName != "" && c.headerVal != "" {
		req.Header.Set(c.headerName, c.headerVal)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &UpstreamError{Message: err.Error(), Err: err}
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, &UpstreamError{StatusCode: resp.StatusCode, Message: err.Error(), Err: err}
	}

	var parsed map[string]any
	if len(respBytes) > 0 {
		_ = json.Unmarshal(respBytes, &parsed)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := extractOutcomeDiagnostics(parsed)
		if msg == "" {
			msg = strings.TrimSpace(string(respBytes))
			if len(msg) > 256 {
				msg = msg[:256]
			}
		}
		var outcome map[string]any
		if rt, _ := parsed["resourceType"].(string); rt == "OperationOutcome" {
			outcome = parsed
		}
		return nil, &UpstreamError{
			StatusCode: resp.StatusCode,
			Outcome:    outcome,
			Message:    msg,
		}
	}

	if parsed == nil {
		return nil, &UpstreamError{
			StatusCode: http.StatusBadGateway,
			Message:    "upstream terminology server returned invalid or empty JSON payload",
		}
	}

	resType, _ := parsed["resourceType"].(string)
	if resType == "OperationOutcome" && expectedResType != "OperationOutcome" {
		return nil, &UpstreamError{
			StatusCode: http.StatusUnprocessableEntity,
			Outcome:    parsed,
			Message:    extractOutcomeDiagnostics(parsed),
		}
	}
	if expectedResType != "" && resType != expectedResType {
		return nil, &UpstreamError{
			StatusCode: http.StatusBadGateway,
			Message:    fmt.Sprintf("expected %s from upstream terminology server, got %q", expectedResType, resType),
		}
	}

	return parsed, nil
}

func extractOutcomeDiagnostics(m map[string]any) string {
	if m == nil {
		return ""
	}
	issues, ok := m["issue"].([]any)
	if !ok {
		return ""
	}
	for _, it := range issues {
		im, isMap := it.(map[string]any)
		if !isMap {
			continue
		}
		if diag, _ := im["diagnostics"].(string); strings.TrimSpace(diag) != "" {
			return strings.TrimSpace(diag)
		}
		if details, isDet := im["details"].(map[string]any); isDet {
			if txt, _ := details["text"].(string); strings.TrimSpace(txt) != "" {
				return strings.TrimSpace(txt)
			}
		}
	}
	return ""
}
