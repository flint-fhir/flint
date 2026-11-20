// Command flintd runs the storage-agnostic Flint FHIR R4 API server over a
// pluggable persistence backend (--store=postgres|sqlite|doris) and optional
// analytical pushdown replica (--analytics-store=doris|sqlite|none).
//
// Usage:
//
//	# Zero-dependency embedded SQLite WAL mode:
//	go run ./cmd/flintd --store=sqlite --sqlite-dsn=flint.db
//
//	# Production PostgreSQL OLTP mode:
//	FLINT_POSTGRES_DSN="postgres://flint:flint@localhost:5432/flint?sslmode=disable" \
//	  go run ./cmd/flintd
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "modernc.org/sqlite"

	"github.com/flint-fhir/flint/automq"
	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/pkg/hl7v2"
	"github.com/flint-fhir/flint/pkg/telemetry"
	"github.com/flint-fhir/flint/pkg/txclient"
	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store"
	"github.com/flint-fhir/flint/store/doris"
	"github.com/flint-fhir/flint/store/postgres"
	"github.com/flint-fhir/flint/store/sqlite"
)

var version = server.Version

type stringListFlag []string

func (f *stringListFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(*f, ",")
}

func (f *stringListFlag) Set(val string) error {
	for part := range strings.SplitSeq(val, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			*f = append(*f, trimmed)
		}
	}
	return nil
}

type cliOptions struct {
	showVersion          bool
	tenantIsolation      string
	storeBackend         string
	analyticsBackend     string
	sqliteDSN            string
	referentialIntegrity string
	igPackages           stringListFlag
	igLoadExamples       bool
	txServerURL          string
	txTimeout            string
	txCacheTTL           string
	txCacheMaxEntries    int
	txAuthHeader         string
	txFailOpen           string
	otlpEndpoint         string
	otlpProtocol         string
	otlpServiceName      string
	otlpSampleRatio      string
	otlpHeaders          string
	mllpAddr             string
	mllpTenant           string
}

func main() {
	logger := slog.New(telemetry.WrapSlogHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	err := run(
		os.Args[1:],
		os.Getenv,
		os.Stdout,
		logger,
		sql.Open,
		http.ListenAndServe,
	)
	if err != nil {
		logger.Error("flintd failed", "error", err)
		os.Exit(1)
	}
}

func parseCLIOptions(args []string, errOut io.Writer) (cliOptions, error) {
	var opts cliOptions
	fs := flag.NewFlagSet("flintd", flag.ContinueOnError)
	if errOut != nil {
		fs.SetOutput(errOut)
	} else {
		fs.SetOutput(io.Discard)
	}
	fs.BoolVar(&opts.showVersion, "version", false, "Print flintd version and exit")
	fs.StringVar(&opts.tenantIsolation, "tenant-isolation", "", "Multi-tenant PostgreSQL isolation mode: 'schema' (default, schema-per-tenant) or 'shared' (shared public tables)")
	fs.StringVar(&opts.storeBackend, "store", "", "Primary operational store backend: 'postgres' (default), 'doris' (Apache Doris MoW), or 'sqlite' (embedded SQLite WAL)")
	fs.StringVar(&opts.analyticsBackend, "analytics-store", "", "Optional analytical store backend: 'doris', 'sqlite', or 'none' (enables hybrid OLTP + OLAP mode)")
	fs.StringVar(&opts.sqliteDSN, "sqlite-dsn", "", "SQLite file path or URI when --store=sqlite (e.g. 'flint.db' or ':memory:')")
	fs.StringVar(&opts.referentialIntegrity, "referential-integrity", "", "Referential integrity policy: 'lenient' (default) or 'strict' (enforces target existence on write and inbound reference conflict on delete)")
	fs.Var(&opts.igPackages, "ig-package", "Path to an HL7 FHIR R4 NPM (.tgz) archive, unpacked IG directory, or directory of .tgz packages (repeatable or comma-separated)")
	fs.BoolVar(&opts.igLoadExamples, "ig-load-examples", false, "Also load and persist clinical example resources from --ig-package archives")
	fs.StringVar(&opts.txServerURL, "tx-server-url", "", "Optional upstream FHIR R4 terminology server base URL (e.g. 'https://tx.fhir.org/r4')")
	fs.StringVar(&opts.txTimeout, "tx-timeout", "", "Per-request timeout for federated terminology calls (e.g. '5s')")
	fs.StringVar(&opts.txCacheTTL, "tx-cache-ttl", "", "In-memory LRU cache TTL for federated terminology responses (e.g. '15m', '-1s' to disable)")
	fs.IntVar(&opts.txCacheMaxEntries, "tx-cache-max-entries", 0, "Maximum cached federated terminology entries (default 4096)")
	fs.StringVar(&opts.txAuthHeader, "tx-auth-header", "", "Optional auth header for federated terminology calls (e.g. 'Bearer <token>' or 'X-Api-Key: <key>')")
	fs.StringVar(&opts.txFailOpen, "tx-fail-open", "", "Fail-open ('true', default) or fail-closed ('false') when upstream terminology server is unreachable")
	fs.StringVar(&opts.otlpEndpoint, "otlp-endpoint", "", "Optional OpenTelemetry OTLP/HTTP collector endpoint (e.g. 'http://otel-collector:4318')")
	fs.StringVar(&opts.otlpProtocol, "otlp-protocol", "", "OpenTelemetry OTLP/HTTP wire protocol: 'http/protobuf' (default) or 'http/json'")
	fs.StringVar(&opts.otlpServiceName, "otlp-service-name", "", "OpenTelemetry service.name attribute (default 'flintd')")
	fs.StringVar(&opts.otlpSampleRatio, "otlp-sample-ratio", "", "OpenTelemetry parent-based root trace sample ratio in [0.0, 1.0] (default '1.0')")
	fs.StringVar(&opts.otlpHeaders, "otlp-headers", "", "Comma-separated OpenTelemetry OTLP HTTP headers (e.g. 'Authorization=Bearer xyz')")
	fs.StringVar(&opts.mllpAddr, "mllp-addr", "", "Optional HL7 v2 MLLP v1 TCP listener address (e.g. ':2575'; disabled when empty)")
	fs.StringVar(&opts.mllpTenant, "mllp-tenant", "", "Target FHIR tenant ID for incoming MLLP connections (default 'default')")
	if err := fs.Parse(args); err != nil {
		return cliOptions{}, err
	}
	return opts, nil
}

func parseFlags(args []string, errOut io.Writer) (showVersion bool, tenantIsolationFlag string, err error) {
	opts, err := parseCLIOptions(args, errOut)
	if err != nil {
		return false, "", err
	}
	return opts.showVersion, opts.tenantIsolation, nil
}

func parseBrokers(rawBrokers string) []string {
	var brokers []string
	for b := range strings.SplitSeq(strings.TrimSpace(rawBrokers), ",") {
		if trimmed := strings.TrimSpace(b); trimmed != "" {
			brokers = append(brokers, trimmed)
		}
	}
	return brokers
}

func resolveIGConfig(cliOpts cliOptions, getenv func(string) string) ([]string, bool) {
	paths := append([]string(nil), cliOpts.igPackages...)
	if envPaths := strings.TrimSpace(getenv("FLINT_IG_PACKAGES")); envPaths != "" {
		for p := range strings.SplitSeq(envPaths, ",") {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				paths = append(paths, trimmed)
			}
		}
	}
	loadExamples := cliOpts.igLoadExamples || strings.EqualFold(strings.TrimSpace(getenv("FLINT_IG_LOAD_EXAMPLES")), "true")
	return paths, loadExamples
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func configureTelemetry(srv *server.Server, cliOpts cliOptions, logger *slog.Logger, getenv func(string) string) error {
	endpoint := firstNonEmpty(
		cliOpts.otlpEndpoint,
		getenv("FLINT_OTLP_ENDPOINT"),
		getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"),
		getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
	)
	if endpoint == "" {
		return nil
	}
	proto := firstNonEmpty(
		cliOpts.otlpProtocol,
		getenv("FLINT_OTLP_PROTOCOL"),
		getenv("OTEL_EXPORTER_OTLP_PROTOCOL"),
	)
	svcName := firstNonEmpty(
		cliOpts.otlpServiceName,
		getenv("FLINT_OTLP_SERVICE_NAME"),
		getenv("OTEL_SERVICE_NAME"),
		"flintd",
	)
	rawRatio := firstNonEmpty(
		cliOpts.otlpSampleRatio,
		getenv("FLINT_OTLP_SAMPLE_RATIO"),
		getenv("OTEL_TRACES_SAMPLER_ARG"),
	)
	ratio, err := telemetry.ParseSampleRatio(rawRatio)
	if err != nil {
		return err
	}
	rawHeaders := firstNonEmpty(
		cliOpts.otlpHeaders,
		getenv("FLINT_OTLP_HEADERS"),
		getenv("OTEL_EXPORTER_OTLP_HEADERS"),
	)
	tr, err := telemetry.NewTracer(telemetry.Config{
		Endpoint:       endpoint,
		Protocol:       proto,
		ServiceName:    svcName,
		ServiceVersion: version,
		Headers:        telemetry.ParseHeaders(rawHeaders),
		SampleRatio:    ratio,
	})
	if err != nil {
		return fmt.Errorf("init opentelemetry tracer: %w", err)
	}
	srv.SetTracer(tr)
	logger.Info("OpenTelemetry OTLP/HTTP distributed tracing enabled", "endpoint", tr.Endpoint(), "protocol", tr.Protocol(), "service_name", svcName)
	return nil
}

func configureTerminologyClient(srv *server.Server, cliOpts cliOptions, logger *slog.Logger, getenv func(string) string) error {
	rawBaseURL := strings.TrimSpace(cliOpts.txServerURL)
	if rawBaseURL == "" {
		rawBaseURL = strings.TrimSpace(getenv("FLINT_TX_SERVER_URL"))
	}
	if rawBaseURL == "" {
		return nil
	}

	var timeout time.Duration
	rawTimeout := strings.TrimSpace(cliOpts.txTimeout)
	if rawTimeout == "" {
		rawTimeout = strings.TrimSpace(getenv("FLINT_TX_TIMEOUT"))
	}
	if rawTimeout != "" {
		d, err := time.ParseDuration(rawTimeout)
		if err != nil {
			return fmt.Errorf("invalid terminology timeout %q: %w", rawTimeout, err)
		}
		timeout = d
	}

	var cacheTTL time.Duration
	rawTTL := strings.TrimSpace(cliOpts.txCacheTTL)
	if rawTTL == "" {
		rawTTL = strings.TrimSpace(getenv("FLINT_TX_CACHE_TTL"))
	}
	if rawTTL != "" {
		d, err := time.ParseDuration(rawTTL)
		if err != nil {
			return fmt.Errorf("invalid terminology cache TTL %q: %w", rawTTL, err)
		}
		cacheTTL = d
	}

	maxEntries := cliOpts.txCacheMaxEntries
	if maxEntries == 0 {
		if rawMax := strings.TrimSpace(getenv("FLINT_TX_CACHE_MAX_ENTRIES")); rawMax != "" {
			n, err := strconv.Atoi(rawMax)
			if err != nil {
				return fmt.Errorf("invalid terminology cache max entries %q: %w", rawMax, err)
			}
			maxEntries = n
		}
	}

	authHeader := strings.TrimSpace(cliOpts.txAuthHeader)
	if authHeader == "" {
		authHeader = strings.TrimSpace(getenv("FLINT_TX_AUTH_HEADER"))
	}

	var failOpen *bool
	rawFailOpen := strings.TrimSpace(cliOpts.txFailOpen)
	if rawFailOpen == "" {
		rawFailOpen = strings.TrimSpace(getenv("FLINT_TX_FAIL_OPEN"))
	}
	if rawFailOpen != "" {
		b, err := strconv.ParseBool(rawFailOpen)
		if err != nil {
			return fmt.Errorf("invalid terminology fail-open %q: %w", rawFailOpen, err)
		}
		failOpen = txclient.BoolPtr(b)
	}

	cli, err := txclient.New(txclient.Config{
		BaseURL:         rawBaseURL,
		Timeout:         timeout,
		CacheTTL:        cacheTTL,
		CacheMaxEntries: maxEntries,
		AuthHeader:      authHeader,
		FailOpen:        failOpen,
	})
	if err != nil {
		return fmt.Errorf("init federated terminology client: %w", err)
	}
	srv.SetTerminologyClient(cli)
	logger.Info("federated FHIR R4 terminology client enabled", "base_url", cli.BaseURL(), "timeout", cli.Timeout().String(), "fail_open", cli.FailOpen())
	return nil
}

func buildStoreOptions(isolationMode postgres.TenantIsolationMode, getenv func(string) string) []postgres.Option {
	storeOpts := []postgres.Option{
		postgres.WithTenantIsolation(isolationMode),
	}
	if strings.EqualFold(strings.TrimSpace(getenv("FLINT_TENANT_ROLE_SWITCH")), "true") {
		storeOpts = append(storeOpts, postgres.WithTenantRoleSwitch(true))
	}
	if strings.EqualFold(strings.TrimSpace(getenv("FLINT_TENANT_AUTO_PROVISION")), "false") {
		storeOpts = append(storeOpts, postgres.WithAutoProvision(false))
	}
	return storeOpts
}

func configureCDSHooksAuth(srv *server.Server, logger *slog.Logger, getenv func(string) string) error {
	if rawReq := strings.TrimSpace(getenv("FLINT_CDS_HOOKS_REQUIRE_JWT")); rawReq != "" {
		req, err := strconv.ParseBool(rawReq)
		if err != nil {
			return fmt.Errorf("invalid FLINT_CDS_HOOKS_REQUIRE_JWT %q: %w", rawReq, err)
		}
		srv.SetCDSHooksRequireJWT(req)
		if req {
			logger.Info("CDS Hooks 2.0 strict RFC 7515 Client JWS enforcement enabled")
		}
	}

	if rawAllow := strings.TrimSpace(getenv("FLINT_CDS_HOOKS_JKU_ALLOWLIST")); rawAllow != "" {
		var prefixes []string
		for part := range strings.SplitSeq(rawAllow, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				prefixes = append(prefixes, trimmed)
			}
		}
		if len(prefixes) > 0 {
			srv.SetCDSHooksAllowedJKUPrefixes(prefixes)
			logger.Info("CDS Hooks 2.0 JKU allowlist configured", "count", len(prefixes))
		}
	}

	if rawJWKS := strings.TrimSpace(getenv("FLINT_CDS_HOOKS_JWKS_JSON")); rawJWKS != "" {
		var single struct {
			Issuer string     `json:"issuer"`
			Keys   []auth.JWK `json:"keys"`
		}
		if err := json.Unmarshal([]byte(rawJWKS), &single); err == nil && strings.TrimSpace(single.Issuer) != "" && len(single.Keys) > 0 {
			srv.RegisterCDSClientJWKS(single.Issuer, auth.JWKSet{Keys: single.Keys})
			logger.Info("CDS Hooks 2.0 static client JWKS registered", "issuer", single.Issuer)
			return nil
		}

		var multi map[string]auth.JWKSet
		if err := json.Unmarshal([]byte(rawJWKS), &multi); err != nil {
			return fmt.Errorf("invalid FLINT_CDS_HOOKS_JWKS_JSON: %w", err)
		}
		for iss, set := range multi {
			if strings.TrimSpace(iss) == "" || len(set.Keys) == 0 {
				return fmt.Errorf("invalid FLINT_CDS_HOOKS_JWKS_JSON entry for issuer %q: non-empty issuer and keys required", iss)
			}
			srv.RegisterCDSClientJWKS(iss, set)
		}
		logger.Info("CDS Hooks 2.0 static client JWKS map registered", "issuers", len(multi))
	}
	return nil
}

func configureAuth(srv *server.Server, logger *slog.Logger, getenv func(string) string) error {
	if err := configureCDSHooksAuth(srv, logger, getenv); err != nil {
		return err
	}
	if strings.EqualFold(strings.TrimSpace(getenv("FLINT_SMART_REQUIRE_CLIENT_JWK")), "true") {
		srv.SetRequireRegisteredClientJWK(true)
	}

	roleToScopes, err := auth.ParseRoleToScopesMap(getenv("FLINT_OIDC_ROLE_TO_SCOPES"))
	if err != nil {
		return fmt.Errorf("invalid FLINT_OIDC_ROLE_TO_SCOPES: %w", err)
	}
	signingAlgs := splitCommaTrimmed(getenv("FLINT_OIDC_SIGNING_ALGS"))
	jwksURL := strings.TrimSpace(getenv("FLINT_OIDC_JWKS_URL"))

	if trustFile := firstNonEmpty(strings.TrimSpace(getenv("FLINT_IDENTITY_TRUST_FILE")), strings.TrimSpace(getenv("PLATFORM_IDENTITY_TRUST_FILE"))); trustFile != "" {
		doc, err := auth.LoadTrustDocFile(trustFile)
		if err != nil {
			return fmt.Errorf("load identity trust file: %w", err)
		}
		var principals []auth.TrustPrincipal
		if rawPrincipals := strings.TrimSpace(getenv("FLINT_TRUST_PRINCIPALS_JSON")); rawPrincipals != "" {
			if err := json.Unmarshal([]byte(rawPrincipals), &principals); err != nil {
				return fmt.Errorf("invalid FLINT_TRUST_PRINCIPALS_JSON: %w", err)
			}
		}
		allowHTTPJWKS, err := parseOptionalBool(getenv("FLINT_TRUST_ALLOW_HTTP_JWKS"), "FLINT_TRUST_ALLOW_HTTP_JWKS")
		if err != nil {
			return err
		}
		val, err := auth.NewTrustDocValidator(auth.TrustDocConfig{
			Document:               doc,
			Principals:             principals,
			RoleToScopes:           roleToScopes,
			AllowPlaintextHTTPJWKS: allowHTTPJWKS,
		})
		if err != nil {
			return fmt.Errorf("init trust doc validator: %w", err)
		}
		srv.SetTokenValidator(val)
		if logger != nil {
			logger.Info("Multi-issuer trust document auth enabled", "issuers", len(doc.Issuers), "principals", len(principals))
		}
		authURL := getenv("FLINT_SMART_AUTH_URL")
		tokenURL := getenv("FLINT_SMART_TOKEN_URL")
		smartIssuer := firstNonEmpty(strings.TrimSpace(getenv("FLINT_OIDC_ISSUER")), doc.Issuers[0].Issuer)
		if authURL != "" && tokenURL != "" {
			srv.SetSMARTConfig(server.SMARTConfig{
				Issuer:                smartIssuer,
				AuthorizationEndpoint: authURL,
				TokenEndpoint:         tokenURL,
			})
		}
		return nil
	}

	oidcIssuer := strings.TrimSpace(getenv("FLINT_OIDC_ISSUER"))
	if oidcIssuer == "" {
		if strings.EqualFold(strings.TrimSpace(getenv("FLINT_SMART_ENFORCE_DEV_AUTH")), "true") {
			srv.EnableDevTokenEnforcement()
			logger.Info("Built-in SMART v2 OAuth2 bearer enforcement enabled")
		}
		return nil
	}
	oidcAudience := getenv("FLINT_OIDC_AUDIENCE")
	idpProvider := getenv("FLINT_AUTH_PROVIDER") // "zitadel" or "oidc"

	var validator auth.TokenValidator
	if idpProvider == "zitadel" {
		zitadelVal, err := auth.NewZitadelValidator(auth.ZitadelConfig{
			OIDCConfig: auth.OIDCConfig{
				Issuer:            oidcIssuer,
				Audience:          oidcAudience,
				JWKSURL:           jwksURL,
				SigningAlgorithms: signingAlgs,
			},
			RoleToScopes:        roleToScopes,
			TenantFromOrgDomain: getenv("FLINT_ZITADEL_TENANT_FROM_DOMAIN") == "true",
		})
		if err != nil {
			return fmt.Errorf("init zitadel validator: %w", err)
		}
		validator = zitadelVal
		logger.Info("SMART on FHIR auth enabled (provider: zitadel)", "issuer", oidcIssuer)
	} else {
		oidcVal, err := auth.NewOIDCValidator(auth.OIDCConfig{
			Issuer:            oidcIssuer,
			Audience:          oidcAudience,
			JWKSURL:           jwksURL,
			SigningAlgorithms: signingAlgs,
		})
		if err != nil {
			return fmt.Errorf("init oidc validator: %w", err)
		}
		validator = oidcVal
		logger.Info("SMART on FHIR auth enabled (provider: oidc)", "issuer", oidcIssuer)
	}

	srv.SetTokenValidator(validator)

	authURL := getenv("FLINT_SMART_AUTH_URL")
	tokenURL := getenv("FLINT_SMART_TOKEN_URL")
	if authURL != "" && tokenURL != "" {
		srv.SetSMARTConfig(server.SMARTConfig{
			Issuer:                oidcIssuer,
			AuthorizationEndpoint: authURL,
			TokenEndpoint:         tokenURL,
		})
	}
	return nil
}

func configureSecurityPolicy(srv *server.Server, logger *slog.Logger, getenv func(string) string) error {
	requireAuth, err := parseOptionalBool(getenv("FLINT_REQUIRE_AUTH"), "FLINT_REQUIRE_AUTH")
	if err != nil {
		return err
	}
	requireTenantClaim, err := parseOptionalBool(getenv("FLINT_REQUIRE_TENANT_CLAIM"), "FLINT_REQUIRE_TENANT_CLAIM")
	if err != nil {
		return err
	}
	disableUnauth, err := parseOptionalBool(getenv("FLINT_DISABLE_UNAUTH_ENDPOINTS"), "FLINT_DISABLE_UNAUTH_ENDPOINTS")
	if err != nil {
		return err
	}
	requireTenantHeader, err := parseOptionalBool(getenv("FLINT_REQUIRE_TENANT_HEADER"), "FLINT_REQUIRE_TENANT_HEADER")
	if err != nil {
		return err
	}
	lockRefIntegrity, err := parseOptionalBool(getenv("FLINT_LOCK_REFERENTIAL_INTEGRITY"), "FLINT_LOCK_REFERENTIAL_INTEGRITY")
	if err != nil {
		return err
	}
	localRefsOnly, err := parseOptionalBool(getenv("FLINT_LOCAL_REFERENCES_ONLY"), "FLINT_LOCAL_REFERENCES_ONLY")
	if err != nil {
		return err
	}
	maxBundleBytes, err := parseOptionalInt64(getenv("FLINT_MAX_BUNDLE_BYTES"), "FLINT_MAX_BUNDLE_BYTES")
	if err != nil {
		return err
	}
	maxBundleEntries, err := parseOptionalInt(getenv("FLINT_MAX_BUNDLE_ENTRIES"), "FLINT_MAX_BUNDLE_ENTRIES")
	if err != nil {
		return err
	}
	rateRPS, err := parseOptionalFloat(getenv("FLINT_RATE_LIMIT_RPS"), "FLINT_RATE_LIMIT_RPS")
	if err != nil {
		return err
	}
	rateBurst, err := parseOptionalInt(getenv("FLINT_RATE_LIMIT_BURST"), "FLINT_RATE_LIMIT_BURST")
	if err != nil {
		return err
	}
	idemTTL, err := parseOptionalDuration(getenv("FLINT_IDEMPOTENCY_TTL"), "FLINT_IDEMPOTENCY_TTL")
	if err != nil {
		return err
	}

	policy := server.SecurityPolicy{
		RequireAuth:                     requireAuth,
		RequireTenantClaim:              requireTenantClaim,
		DisableUnauthenticatedEndpoints: disableUnauth,
		AllowedOrigins:                  splitCommaTrimmed(getenv("FLINT_ALLOWED_ORIGINS")),
		TenantHeaderName:                strings.TrimSpace(getenv("FLINT_TENANT_HEADER")),
		RequireTenantHeader:             requireTenantHeader,
		LockReferentialIntegrity:        lockRefIntegrity,
		EnforceLocalReferencesOnly:      localRefsOnly,
		MaxBundleBytes:                  maxBundleBytes,
		MaxBundleEntries:                maxBundleEntries,
		DeniedResourceTypes:             splitCommaTrimmed(getenv("FLINT_DENIED_RESOURCE_TYPES")),
		RateLimitPerSecond:              rateRPS,
		RateLimitBurst:                  rateBurst,
		IdempotencyTTL:                  idemTTL,
	}
	srv.SetSecurityPolicy(policy)
	if err := srv.ValidateSecurityPolicy(); err != nil {
		return err
	}
	if logger != nil && (policy.RequireAuth || policy.EnforceLocalReferencesOnly || policy.LockReferentialIntegrity || policy.RateLimitPerSecond > 0) {
		logger.Info("Security policy hardened",
			"require_auth", policy.RequireAuth,
			"require_tenant_claim", policy.RequireTenantClaim,
			"local_references_only", policy.EnforceLocalReferencesOnly,
			"lock_referential_integrity", policy.LockReferentialIntegrity,
			"rate_limit_rps", policy.RateLimitPerSecond,
		)
	}
	return nil
}

func splitCommaTrimmed(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func parseOptionalBool(raw, name string) (bool, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false, nil
	}
	v, err := strconv.ParseBool(trimmed)
	if err != nil {
		return false, fmt.Errorf("invalid %s %q: %w", name, raw, err)
	}
	return v, nil
}

func parseOptionalInt(raw, name string) (int, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, nil
	}
	v, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", name, raw, err)
	}
	return v, nil
}

func parseOptionalInt64(raw, name string) (int64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", name, raw, err)
	}
	return v, nil
}

func parseOptionalFloat(raw, name string) (float64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, nil
	}
	v, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", name, raw, err)
	}
	return v, nil
}

func parseOptionalDuration(raw, name string) (time.Duration, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, nil
	}
	v, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", name, raw, err)
	}
	return v, nil
}

func openDorisStore(getenv func(string) string, openDB func(driverName, dsn string) (*sql.DB, error)) (*doris.Store, func(), error) {
	groupCommit, err := doris.ParseGroupCommitMode(getenv("FLINT_DORIS_GROUP_COMMIT"))
	if err != nil {
		return nil, nil, err
	}
	rawIsolation := strings.TrimSpace(getenv("FLINT_DORIS_TENANT_ISOLATION"))
	if rawIsolation == "" {
		rawIsolation = strings.TrimSpace(getenv("FLINT_TENANT_ISOLATION"))
	}
	isolation, err := doris.ParseTenantIsolationMode(rawIsolation)
	if err != nil {
		return nil, nil, err
	}
	autoProvision := isolation == doris.TenantIsolationDatabase
	if rawAuto := strings.TrimSpace(getenv("FLINT_DORIS_AUTO_PROVISION")); rawAuto != "" {
		autoProvision, err = parseOptionalBool(rawAuto, "FLINT_DORIS_AUTO_PROVISION")
		if err != nil {
			return nil, nil, err
		}
	}
	repNum := 1
	if rawRep := strings.TrimSpace(getenv("FLINT_DORIS_REPLICATION_NUM")); rawRep != "" {
		repNum, err = parseOptionalInt(rawRep, "FLINT_DORIS_REPLICATION_NUM")
		if err != nil {
			return nil, nil, err
		}
		if repNum < 1 {
			return nil, nil, fmt.Errorf("invalid FLINT_DORIS_REPLICATION_NUM %q: must be >= 1", rawRep)
		}
	}
	requireNonRoot, err := parseOptionalBool(getenv("FLINT_DORIS_REQUIRE_NON_ROOT"), "FLINT_DORIS_REQUIRE_NON_ROOT")
	if err != nil {
		return nil, nil, err
	}
	execMemLimit, err := parseOptionalInt64(getenv("FLINT_DORIS_EXEC_MEM_LIMIT"), "FLINT_DORIS_EXEC_MEM_LIMIT")
	if err != nil {
		return nil, nil, err
	}
	cpuThreads, err := parseOptionalInt(getenv("FLINT_DORIS_CPU_THREADS"), "FLINT_DORIS_CPU_THREADS")
	if err != nil {
		return nil, nil, err
	}
	queryTimeout, err := parseOptionalInt(getenv("FLINT_DORIS_QUERY_TIMEOUT"), "FLINT_DORIS_QUERY_TIMEOUT")
	if err != nil {
		return nil, nil, err
	}
	limits := doris.ResourceLimits{
		WorkloadGroup:   strings.TrimSpace(getenv("FLINT_DORIS_WORKLOAD_GROUP")),
		ComputeGroup:    strings.TrimSpace(getenv("FLINT_DORIS_COMPUTE_GROUP")),
		ExecMemLimit:    execMemLimit,
		CPUThreads:      cpuThreads,
		QueryTimeoutSec: queryTimeout,
	}
	if err := limits.Validate(); err != nil {
		return nil, nil, err
	}

	dbName := strings.TrimSpace(getenv("FLINT_DORIS_DATABASE"))
	var (
		dorisDB                *sql.DB
		lockDB                 *sql.DB
		sessionGroupConfigured bool
	)
	closeDBs := func() {
		if lockDB != nil {
			_ = lockDB.Close()
		}
		if dorisDB != nil {
			_ = dorisDB.Close()
		}
	}

	if dorisDSN := strings.TrimSpace(getenv("FLINT_DORIS_DSN")); dorisDSN != "" {
		normalizedDSN, err := doris.NormalizeDSN(dorisDSN, doris.DSNOptions{
			DefaultDatabase: dbName,
			GroupCommit:     groupCommit,
			RequireNonRoot:  requireNonRoot,
		})
		if err != nil {
			return nil, nil, err
		}
		dorisDB, err = openDB("mysql", normalizedDSN)
		if err != nil {
			return nil, nil, fmt.Errorf("open doris db: %w", err)
		}
		doris.ConfigurePool(dorisDB, 0, 0, 0, 0)
		if err := dorisDB.Ping(); err != nil {
			closeDBs()
			return nil, nil, fmt.Errorf("ping doris db: %w", err)
		}
		if err := doris.RunMigrationsWithOptions(context.Background(), dorisDB, doris.MigrationOptions{
			Database:        dbName,
			TenantIsolation: isolation,
			ReplicationNum:  repNum,
		}); err != nil {
			closeDBs()
			return nil, nil, fmt.Errorf("run doris migrations: %w", err)
		}
		sessionGroupConfigured = true
	}

	opts := []doris.Option{
		doris.WithGroupCommit(groupCommit),
		doris.WithDatabase(dbName),
		doris.WithTenantIsolation(isolation),
		doris.WithAutoProvision(autoProvision),
		doris.WithReplicationNum(repNum),
		doris.WithResourceLimits(limits),
		doris.WithSessionGroupCommitConfigured(sessionGroupConfigured),
	}

	if lockPGDSN := strings.TrimSpace(getenv("FLINT_DORIS_LOCK_POSTGRES_DSN")); lockPGDSN != "" {
		lockDB, err = openDB("postgres", lockPGDSN)
		if err != nil {
			closeDBs()
			return nil, nil, fmt.Errorf("open doris lock postgres db: %w", err)
		}
		if err := lockDB.Ping(); err != nil {
			closeDBs()
			return nil, nil, fmt.Errorf("ping doris lock postgres db: %w", err)
		}
		locker, err := doris.NewPostgresRowLocker(lockDB)
		if err != nil {
			closeDBs()
			return nil, nil, fmt.Errorf("create doris row locker: %w", err)
		}
		if err := locker.EnsureTable(context.Background()); err != nil {
			closeDBs()
			return nil, nil, fmt.Errorf("ensure doris row lock table: %w", err)
		}
		opts = append(opts, doris.WithRowLocker(locker))
	}

	ds := doris.New(dorisDB, opts...)
	return ds, closeDBs, nil
}

func openSQLiteStore(cliDSN string, getenv func(string) string, openDB func(driverName, dsn string) (*sql.DB, error)) (*sqlite.Store, *sql.DB, error) {
	journalMode, err := sqlite.ParseJournalMode(getenv("FLINT_SQLITE_JOURNAL_MODE"))
	if err != nil {
		return nil, nil, err
	}
	sqliteDSN := strings.TrimSpace(cliDSN)
	if sqliteDSN == "" {
		sqliteDSN = strings.TrimSpace(getenv("FLINT_SQLITE_DSN"))
	}
	var sqliteDB *sql.DB
	if sqliteDSN != "" && sqliteDSN != ":memory:" {
		sqliteDB, err = openDB("sqlite", sqliteDSN)
		if err != nil {
			return nil, nil, fmt.Errorf("open sqlite db: %w", err)
		}
		sqliteDB.SetMaxOpenConns(1)
		if err := sqliteDB.Ping(); err != nil {
			_ = sqliteDB.Close()
			return nil, nil, fmt.Errorf("ping sqlite db: %w", err)
		}
		if err := sqlite.RunMigrations(context.Background(), sqliteDB, journalMode); err != nil {
			_ = sqliteDB.Close()
			return nil, nil, fmt.Errorf("run sqlite migrations: %w", err)
		}
	}
	ss := sqlite.New(
		sqliteDB,
		sqlite.WithJournalMode(journalMode),
		sqlite.WithDSN(sqliteDSN),
	)
	if sqliteDB != nil {
		if err := ss.LoadFromDB(context.Background()); err != nil {
			_ = sqliteDB.Close()
			return nil, nil, fmt.Errorf("load sqlite state from db: %w", err)
		}
	}
	return ss, sqliteDB, nil
}

func run(
	args []string,
	getenv func(string) string,
	out io.Writer,
	logger *slog.Logger,
	openDB func(driverName, dsn string) (*sql.DB, error),
	serve func(addr string, handler http.Handler) error,
) error {
	cliOpts, err := parseCLIOptions(args, out)
	if err != nil {
		return err
	}
	if cliOpts.showVersion {
		_, _ = fmt.Fprintf(out, "flintd %s\n", version)
		return nil
	}

	rawStore := strings.TrimSpace(cliOpts.storeBackend)
	if rawStore == "" {
		rawStore = strings.TrimSpace(getenv("FLINT_STORE_BACKEND"))
	}
	storeBackend, err := store.ParseBackendType(rawStore)
	if err != nil {
		return fmt.Errorf("invalid store backend: %w", err)
	}

	rawAnalytics := strings.TrimSpace(cliOpts.analyticsBackend)
	if rawAnalytics == "" {
		rawAnalytics = strings.TrimSpace(getenv("FLINT_ANALYTICS_BACKEND"))
	}
	analyticsBackend, err := store.ParseOptionalBackendType(rawAnalytics)
	if err != nil {
		return fmt.Errorf("invalid analytics backend: %w", err)
	}

	rawRefIntegrity := strings.TrimSpace(cliOpts.referentialIntegrity)
	if rawRefIntegrity == "" {
		rawRefIntegrity = strings.TrimSpace(getenv("FLINT_REFERENTIAL_INTEGRITY"))
	}
	refIntegrityMode, err := server.ParseReferentialIntegrityMode(rawRefIntegrity)
	if err != nil {
		return fmt.Errorf("invalid referential integrity mode: %w", err)
	}

	var primaryStore store.Store
	var analyticalStore store.AnalyticalStore

	switch storeBackend {
	case store.BackendDoris:
		ds, cleanupDoris, err := openDorisStore(getenv, openDB)
		if err != nil {
			return err
		}
		if cleanupDoris != nil {
			defer cleanupDoris()
		}
		primaryStore = ds
		analyticalStore = ds
		logger.Info("Zero-JSON Apache Doris store initialized",
			"group_commit", string(ds.GroupCommit()),
			"database", ds.Database(),
			"tenant_isolation", string(ds.TenantIsolation()),
		)
	case store.BackendSQLite:
		ss, sqliteDB, err := openSQLiteStore(cliOpts.sqliteDSN, getenv, openDB)
		if err != nil {
			return err
		}
		if sqliteDB != nil {
			defer sqliteDB.Close()
		}
		primaryStore = ss
		analyticalStore = ss
		logger.Info("Embedded Zero-JSON SQLite WAL store initialized", "journal_mode", string(ss.JournalMode()), "dsn", ss.DSN())
	default:
		rawIsolation := strings.TrimSpace(cliOpts.tenantIsolation)
		if rawIsolation == "" {
			rawIsolation = strings.TrimSpace(getenv("FLINT_TENANT_ISOLATION"))
		}
		isolationMode, err := postgres.ParseTenantIsolationMode(rawIsolation)
		if err != nil {
			return fmt.Errorf("invalid tenant isolation mode: %w", err)
		}

		dsn := getenv("FLINT_POSTGRES_DSN")
		if dsn == "" {
			dsn = "postgres://flint:flint@localhost:5432/flint?sslmode=disable"
		}

		db, err := openDB("postgres", dsn)
		if err != nil {
			return fmt.Errorf("open db: %w", err)
		}
		defer db.Close()

		if err := db.Ping(); err != nil {
			return fmt.Errorf("ping db: %w", err)
		}

		if err := postgres.RunMigrations(context.Background(), db); err != nil {
			return fmt.Errorf("run migrations: %w", err)
		}

		storeOpts := buildStoreOptions(isolationMode, getenv)
		pgStore := postgres.New(db, storeOpts...)
		primaryStore = pgStore
		logger.Info("PostgreSQL store initialized", "tenant_isolation", string(isolationMode))

		switch analyticsBackend {
		case store.BackendDoris:
			ds, cleanupDoris, err := openDorisStore(getenv, openDB)
			if err != nil {
				return err
			}
			if cleanupDoris != nil {
				defer cleanupDoris()
			}
			analyticalStore = ds
			logger.Info("Hybrid OLTP+OLAP mode enabled: Apache Doris analytical store attached",
				"group_commit", string(ds.GroupCommit()),
				"tenant_isolation", string(ds.TenantIsolation()),
			)
		case store.BackendSQLite:
			ss, sqliteDB, err := openSQLiteStore(cliOpts.sqliteDSN, getenv, openDB)
			if err != nil {
				return err
			}
			if sqliteDB != nil {
				defer sqliteDB.Close()
			}
			analyticalStore = ss
			logger.Info("Hybrid OLTP+OLAP mode enabled: SQLite analytical store attached", "journal_mode", string(ss.JournalMode()), "dsn", ss.DSN())
		}
	}

	srv := server.New(primaryStore, logger)
	defer func() { _ = srv.Close() }()
	srv.SetReferentialIntegrity(refIntegrityMode)
	if analyticalStore != nil {
		srv.SetAnalyticalStore(analyticalStore)
	}
	if err := configureTelemetry(srv, cliOpts, logger, getenv); err != nil {
		return err
	}

	// Register all supported US Core / USCDI v3 FHIR R4 resource types
	server.RegisterDefaultResourceTypes(srv)

	if strings.EqualFold(strings.TrimSpace(getenv("FLINT_ALLOW_LOCAL_IG_PATH_INSTALL")), "true") {
		srv.SetAllowLocalIGPathInstall(true)
	}

	if igPaths, loadExamples := resolveIGConfig(cliOpts, getenv); len(igPaths) > 0 {
		igTenant := strings.TrimSpace(getenv("FLINT_IG_TENANT"))
		if igTenant == "" {
			igTenant = "default"
		}
		summaries, err := srv.LoadIGPaths(context.Background(), igPaths, server.InstallIGOptions{
			Tenant:       igTenant,
			Persist:      !store.IsNil(primaryStore),
			LoadExamples: loadExamples,
		})
		if err != nil {
			return fmt.Errorf("load ig packages: %w", err)
		}
		logger.Info("startup FHIR Implementation Guide packages loaded", "count", len(summaries), "tenant", igTenant)
	}

	if err := configureTerminologyClient(srv, cliOpts, logger, getenv); err != nil {
		return err
	}

	// Optional AutoMQ / Kafka CDC producer configuration
	if brokers := parseBrokers(getenv("FLINT_AUTOMQ_BROKERS")); len(brokers) > 0 {
		schemaRegistryURL := strings.TrimSpace(getenv("FLINT_SCHEMA_REGISTRY_URL"))
		cdcProducer, err := automq.NewProducer(automq.Config{
			Brokers:           brokers,
			TopicPrefix:       "fhir.",
			SchemaRegistryURL: schemaRegistryURL,
			Logger:            logger,
		})
		if err != nil {
			return fmt.Errorf("init automq cdc producer: %w", err)
		}
		defer cdcProducer.Close()
		srv.SetCDCPublisher(cdcProducer)
		logger.Info("AutoMQ CDC streaming enabled", "brokers", brokers, "schemaRegistryURL", schemaRegistryURL)
	}

	// Optional SMART on FHIR Auth & OIDC/Zitadel/TrustDoc configuration
	if err := configureAuth(srv, logger, getenv); err != nil {
		return err
	}
	if err := configureSecurityPolicy(srv, logger, getenv); err != nil {
		return err
	}

	mllpSrv, err := startMLLPServerIfConfigured(srv, cliOpts, logger, getenv)
	if err != nil {
		return err
	}
	if mllpSrv != nil {
		defer func() { _ = mllpSrv.Close() }()
	}

	addr := getenv("FLINT_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	logger.Info("starting flint", "addr", addr)
	if err := serve(addr, srv.Handler()); err != nil {
		return fmt.Errorf("server: %w", err)
	}
	return nil
}

func startMLLPServerIfConfigured(srv *server.Server, cliOpts cliOptions, logger *slog.Logger, getenv func(string) string) (*hl7v2.MLLPServer, error) {
	mllpAddr := firstNonEmpty(cliOpts.mllpAddr, getenv("FLINT_MLLP_ADDR"))
	if mllpAddr == "" {
		return nil, nil
	}
	mllpTenant := firstNonEmpty(cliOpts.mllpTenant, getenv("FLINT_MLLP_TENANT"), "default")
	mllpSrv := srv.NewMLLPServer(mllpAddr, mllpTenant)
	if err := mllpSrv.Start(); err != nil {
		return nil, fmt.Errorf("start hl7v2 mllp listener on %q: %w", mllpAddr, err)
	}
	if logger != nil {
		logger.Info("HL7 v2 MLLP TCP ingestion listener started", "addr", mllpSrv.Addr(), "tenant", mllpTenant)
	}
	return mllpSrv, nil
}
