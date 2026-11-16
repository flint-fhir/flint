# Changelog

All notable changes to **Flint** will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **HL7 International Patient Summary (`ips_test_kit`), Da Vinci Plan-Net (`davinci_plan_net_test_kit`) & US Drug Formulary (`davinci_us_drug_formulary_test_kit`) Headless Inferno Conformance (`server/ips_davinci.go`, `store/`, `cmd/seed`, `deploy/inferno/`)**:
  - Upgraded `Patient/{id}/$summary` (`Bundle-uv-ips` / `Composition-uv-ips`) with absolute `Bundle.entry.fullUrl` URIs, RFC 4122 `urn:uuid:` document identifiers, `Composition.text`, problem-list `Condition` filtering, and 1-hop target reference closure, passing `ips_test_kit` (`ips_v110`) headlessly on PostgreSQL and SQLite.
  - Added full Da Vinci PDex Plan-Net v1.1.0 (`davinci_plan_net_server_v110`) and US Drug Formulary v2.0.1 (`davinci_us_drug_formulary_v201`) search parameter extraction (`PractitionerRole?network`, `Organization?coverage-area`, `InsurancePlan?formulary-coverage` / `plan-type` / `coverage-type`, `Basic?formulary` / `status` / `period` / `pharmacy-benefit-type` / `drug-tier`, `MedicationKnowledge?drug-name` / `doseform`, polymorphic `Location?partof.*` forward chaining, `HealthcareService?service-category` / `service-type`, `OrganizationAffiliation?primary-organization` / `service`), `CapabilityStatement` profile/`instantiates` advertisement, and deterministic `-suite plannet` / `-suite formulary` cohorts in `cmd/seed`.
- **HL7 FHIR R4 eCQM / Da Vinci DEQM `Measure/$evaluate-measure` Stratifiers, Supplemental Data & Report Modes (`server/cqm_stratifiers_sde.go`, `server/cqm_cds.go`)**:
  - Added single-component (`Measure.group.stratifier.criteria` → `stratum.value`) and multi-component (`Measure.group.stratifier.component[]` → `stratum.component[]`) FHIRPath cohort stratification enforcing the FHIR R4 `value.exists() xor component.exists()` invariant, per-stratum population counts (`initial-population`, `denominator`, `denominator-exclusion`, `denominator-exception`, `numerator`, `numerator-exclusion`), and proportion `measureScore` calculation.
  - Added `Measure.supplementalData` (`sde-sex`, `sde-race`, `sde-ethnicity`, `sde-payer`, and custom FHIRPath SDEs) evaluation emitting summarized `Observation` resources (`cqf-measureInfo` extension + `component[count].valueInteger`) in `MeasureReport.contained` linked via `extension-criteriaReference` in `MeasureReport.evaluatedResource`.
  - Added full `reportType=individual`, `reportType=subject-list` (contained `List` resources per group population and per stratum population), `reportType=summary`, and `subject=Group/{id}` cohort filtering across FHIR JSON, FHIR R4 XML (`application/fhir+xml`), and binary Proto3 (`application/x-protobuf`) wire formats, backed by Hegel stratifier partition conservation and `subject-list` bijection property tests (`server/ecqm_stratifiers_sde_test.go`).
- **HL7 Da Vinci HRex `Patient/$member-match` & CDS Hooks 2.0 Cryptographic JWS Verification (`server/ips_davinci.go`, `server/cdshooks_jwt.go`, `pkg/auth/jwt.go`, `cmd/flintd`)**:
  - Upgraded `POST /fhir/r4/{tenant}/Patient/$member-match` to full Da Vinci HRex STU 1.0/1.1 conformance supporting `MemberPatient`, `CoverageToMatch`, optional `CoverageToLink` persistence, and optional `Consent` active/permit/period validation & persistence, deterministic DOB/coverage conflict guards, `422 Unprocessable Entity` on zero or ambiguous multi-matches, JSON/XML/Proto3 wire negotiation, and ASTM E2147 `AuditEvent` patient attribution.
  - Added RFC 7515 asymmetric `ES256`, `ES384`, and `RS256` CDS Client JWS caller verification (`iss`, `aud`, `exp`, `iat`, atomic `jti` replay cache, static `FLINT_CDS_HOOKS_JWKS_JSON` trust anchors, SSRF-hardened `FLINT_CDS_HOOKS_JKU_ALLOWLIST` dynamic `jku` fetching with TTL cache, and `FLINT_CDS_HOOKS_REQUIRE_JWT` strict mode) across all CDS Hooks 2.0 discovery, invocation, `$dsi-attributes`, and `/feedback` endpoints with ASTM E2147 `AuditEvent` logging on verification failures.
- **Expanded Headless HL7 Inferno Conformance CI — US Core v8.0.0, ONC `(g)(10)` HTI-1 & Subscriptions R5 Backport ([`.github/workflows/inferno.yml`](.github/workflows/inferno.yml), [`deploy/inferno/headless/`](deploy/inferno/headless/))**:
  - Added **US Core Server v8.0.0 (USCDI v5)** (`us_core_v800` in `us_core_test_kit ~> 1.2`), including `us-core-adi-documentreference` and `us-core-observation-adi-documentation` profile validation, `CapabilityStatement` advertisement, and deterministic cohort fixtures (`cmd/seed`).
  - Added **ONC § 170.315(g)(10) Standardized API Certification** (`g10_certification` in `onc_certification_g10_test_kit ~> 8.1`) across standalone/EHR launches, token revocation, invalid `aud`/PKCE rejection, SMART v2 fine-grained scopes, asymmetric launch, and Bulk Data v2.0.0.
  - Added **HL7 Subscriptions R5 Backport IG v1.1.0 FHIR R4 Server Suite** (`subscriptions_r5_backport_r4_server` in `subscriptions_test_kit ~> 0.13`) verifying live `rest-hook` handshake/heartbeat/event-notification `Bundle` delivery, `CapabilityStatement` topic discovery, `$status`, and invalid subscription rejection across PostgreSQL and SQLite.
- **Stateful Differential & Wire-Isomorphism Property-Based Testing (`store/`, `pkg/fhirxml`, `pkg/txclient`, `server/`)**:
  - Added `hegel.RunStateful` differential state-machine verification across `postgres`, `sqlite` (including `LoadFromDB` crash recovery), `doris`, and `HybridStore`, plus UCUM and `_filter` Boolean algebra PBTs (`store/differential_property_test.go`).
  - Added 4-way lossless `FHIR JSON <-> FHIR R4 XML <-> google/fhir Proto3` wire isomorphism PBTs (including exact `Decimal` trailing-zero lexeme preservation) and DTD/ENTITY XXE rejection (`pkg/fhirxml/property_test.go`, `pkg/fhirutil/json_codec.go`).
  - Added live SQL-on-FHIR v2 `ViewDefinition/$run` vs `ViewDefinition/$sql?dialect=sqlite` differential execution oracle, FHIRPath Kleene 3-valued logic laws, RFC 6455 WebSocket framing/fragmentation PBTs, Patch/Transaction atomicity PBTs, and `pkg/txclient` stateful LRU/TTL cache verification.
- **Continuous Native Fuzzing, Concurrency Contention, `govulncheck` & XML `$certify` Hardening (`.github/workflows/ci.yml`, `.github/workflows/fuzz.yml`, `server/concurrency_hardening_test.go`, `pkg/certify`)**:
  - Wired `FuzzXMLRoundTrip` and `FuzzWebSocketFrames` into CI alongside a scheduled deep-fuzz workflow, `govulncheck ./...` static vulnerability gate, parallel `-race` optimistic locking (`If-Match` / `CreateOnly` / `If-None-Exist`) contention tests, and `application/fhir+xml` `$certify` checks.

### Fixed

- Wrapped `sqlite.Store.ExecuteTransactionBundle` in an explicit ACID `sql.Tx` and synchronized SQLite SQL table deletes in `PurgePatientCompartment` and `ExpungeResources` so aborted bundle rollbacks and `LoadFromDB` crash recovery match in-memory state identically (`store/sqlite/store.go`).
- Fixed prefix `not(expr)` evaluation in `evalFHIRPathExpr` (`ViewDefinition/$run`) to match `ViewDefinition/$sql` `NOT (...)` transpilation (`server/fhirpath_engine.go`).

## [0.5.0] - 2026-11-14

### Added

- **Bidirectional HL7 FHIR R4 XML Wire Format (`pkg/fhirxml`, `server/xml_wire.go`)**:
  - Added pure-Go, zero-external-dependency HL7 FHIR R4 XML (`http://hl7.org/fhir`) ↔ FHIR JSON / `google/fhir` Proto3 transcoder (`pkg/fhirxml`) covering all 146 R4 resource types, `value="..."` primitive attributes, shadow `_field` primitive `id`/`extension` elements, `Extension.url` XML attributes, raw `Narrative.div` XHTML (`http://www.w3.org/1999/xhtml`), inline `<contained>` and `<Bundle><entry><resource>` wrappers, polymorphic `[x]` choice elements, deterministic `StructureDefinition` element ordering, XXE/DOCTYPE/ENTITY & depth-limit hardening, and round-trip fuzzing (`FuzzXMLRoundTrip`).
  - Wired end-to-end HTTP content negotiation (`Accept: application/fhir+xml`, `application/xml`, `text/xml` with `;q=` quality factors, `Content-Type` fallback, and `?_format=xml|application/fhir+xml`) across CRUD, `vread`, `_history`, Search, `batch`/`transaction` `Bundle`, `PATCH`, `$validate`, `$convert`, terminology operations, `PlanDefinition/$apply`, `OperationOutcome` diagnostics, and `CapabilityStatement.format`.
- **Real-Time FHIR R4 & R5 Backport Subscriptions Push Streaming — WebSockets & CloudEvents v1.0 (`server/subscriptions.go`)**:
  - Added pure-Go RFC 6455 WebSocket `Subscription` channel (`channel.type = "websocket"`) on `GET /fhir/r4/$subscriptions-ws` and `GET /fhir/r4/{tenant}/$subscriptions-ws`, supporting direct `bind <id>` and R5 Backport token-bound `bind : <token>` (`Subscription/$bind-ws` & `$get-ws-binding-token`), initial `bound <id>` / `handshake` `SubscriptionStatus` frames, periodic `ping <id>` keepalive heartbeats, real-time R5 Backport `history` notification `Bundle` (`empty`, `id-only`, `full-resource`) push delivery, and tenant isolation.
  - Added CNCF CloudEvents v1.0 (`application/cloudevents+json` and `application/cloudevents-batch+json`) `rest-hook` webhook envelope formatting (`specversion: "1.0"`, `type: "org.hl7.fhir.r4.subscription.<ResourceType>"`, `source`, `subject`, `datacontenttype`, `data`) with `flint_subscription_ws_active_connections` and `flint_subscription_ws_messages_sent_total` Prometheus metrics.
- **External FHIR R4 Terminology Server Federation (`pkg/txclient`, `server/terminology.go`)**:
  - Added thread-safe, bounded-timeout external FHIR R4 terminology federation client (`pkg/txclient`) with in-memory LRU + TTL caching (`--tx-server-url` / `FLINT_TX_SERVER_URL`, `--tx-timeout` / `FLINT_TX_TIMEOUT`, `--tx-cache-ttl` / `FLINT_TX_CACHE_TTL`, `--tx-cache-max-entries` / `FLINT_TX_CACHE_MAX_ENTRIES`, `--tx-auth-header` / `FLINT_TX_AUTH_HEADER`, `--tx-fail-open` / `FLINT_TX_FAIL_OPEN`).
  - Wired transparent upstream fallback delegation across `ValueSet/$expand`, `ValueSet/$validate-code`, `CodeSystem/$lookup`, `CodeSystem/$validate-code`, `CodeSystem/$subsumes`, `:in` / `:not-in` terminology search modifiers, and `strength: "required"` `ElementDefinition.binding` `ValueSet` validation when code systems or value sets are not locally loaded, plus `flint_tx_federation_requests_total` and `flint_tx_federation_cache_hits_total` Prometheus metrics.
- **Full HL7 Inferno SMART App Launch STU2 Conformance (`deploy/inferno/headless/run_suites.rb`, `server/smart.go`)**:
  - Automated headless OAuth2/PKCE (`S256`) browser-redirect, EHR launch (`/oauth2/launch`), OpenID Connect `id_token` (`fhirUser`), refresh token rotation & downscoping, token introspection/revocation, and confidential asymmetric `ES384` `private_key_jwt` standalone launch execution in HL7 Inferno CI (`smart_stu2` 60/60 + `smart_stu2_asymmetric` 20/20 — **896 passing leaf tests** across PostgreSQL and SQLite).

### Changed

- Synchronized runtime version reporting (`0.5.0`) across `flintd --version`, `CapabilityStatement.software.version`, OpenAPI 3.1.0 `info.version`, `Dockerfile`, `analytics/pyproject.toml`, `install.sh`, `README.md`, and `site/`.

### Fixed

- Bootstrapped public [`flint-fhir/homebrew-tap`](https://github.com/flint-fhir/homebrew-tap) (`brew install flint-fhir/tap/flint`) and fixed job-level `env: HOMEBREW_TAP_TOKEN` conditional evaluation in [`.github/workflows/release.yml`](.github/workflows/release.yml).

## [0.4.0] - 2026-11-13

### Added

- **HL7 FHIR R4 NPM (`.tgz`) Implementation Guide Package Loader (`pkg/igloader`, `server/ig_ops.go`)**:
  - Added pure-Go `.tgz` / `package/` HL7 NPM archive loader (`pkg/igloader`) with streaming decompression caps, path-traversal/symlink rejection, `fhirVersions` compatibility checks (`4.0.x`), SHA-256 package digests, and native Go fuzzing (`FuzzLoadTGZBytes`).
  - Added startup CLI flags (`--ig-package`, `--ig-load-examples`) and environment variables (`FLINT_IG_PACKAGES`, `FLINT_IG_LOAD_EXAMPLES`, `FLINT_IG_TENANT`, `FLINT_ALLOW_LOCAL_IG_PATH_INSTALL`) to boot `flintd` with pre-installed national, specialty, or custom enterprise IGs.
  - Added runtime HTTP operations (`POST /fhir/r4/{tenant}/$install-ig`, `POST /fhir/r4/{tenant}/$load-ig`, `GET /fhir/r4/{tenant}/$igs`) with automatic `StructureDefinition` validator registration, `CodeSystem`/`ValueSet` terminology indexing, `PlanDefinition`/`ActivityDefinition`/`Library`/`Questionnaire`/`Measure` artifact persistence, custom `SearchParameter` compilation & optional historical `$reindex`, live `CapabilityStatement.implementationGuide` / `rest.resource.supportedProfile` advertisement, and ASTM E2147 `AuditEvent` recording.
  - Added exact versioned `url|version` `StructureDefinition` profile resolution and `strength: "required"` `ElementDefinition.binding` `ValueSet` membership enforcement during `$validate` and profile-constrained writes.
- **Full FHIR R4 Structural, FHIRPath Semantic & Referential Integrity Conformance**:
  - Added compile-time FHIRPath R4 AST semantic type-checking (`ValidateSemantics`) enforcing `StructureDefinition` element existence and operator compatibility prior to runtime evaluation.
  - Added `StructureDefinition` element slicing & discriminator validation (`value`, `pattern`, `type`, `profile`, `exists`, plus `slicing.rules = "closed"` rejection and per-slice `min`/`max` cardinality enforcement).
  - Added strict referential integrity mode (`--referential-integrity=strict` / `FLINT_REFERENTIAL_INTEGRITY=strict`) rejecting dangling local references on write (`422 Unprocessable Entity`), enforcing intra-transaction `Bundle` reference ordering, and blocking deletes of inbound-referenced targets (`409 Conflict`) unless `?_cascade=delete` is requested.
  - Expanded headless **HL7 Inferno Conformance CI** ([`.github/workflows/inferno.yml`](.github/workflows/inferno.yml), [`deploy/inferno/headless/run_suites.rb`](deploy/inferno/headless/run_suites.rb)) to execute **US Core Server v7.0.0** (USCDI v4 — all 26 profiles) and **HL7 International Patient Access (IPA) v1.0.0** alongside US Core v6.1.0, SMART App Launch STU2, and Bulk Data v2.0.0 on both PostgreSQL and SQLite.
- **Pure-Go Embedded SDK, Homebrew Tap & Zero-Dependency POSIX Installer**:
  - Added [`examples/embedded-sqlite/`](examples/embedded-sqlite/) demonstrating `CGO_ENABLED=0` library embedding of `server.Server` + `store/sqlite` inside standard `net/http` Go applications (verified in CI).
  - Added SHA-256 verified POSIX `install.sh` installer and automated multi-arch (`darwin/arm64`, `darwin/amd64`, `linux/arm64`, `linux/amd64`) GitHub Release archive & Homebrew tap (`flint-fhir/tap/flint`) publishing in [`.github/workflows/release.yml`](.github/workflows/release.yml).

### Changed

- **Physical Decoupling of Storage Domain & Search Extractors (`store/`, `gen/go/searchidx/`)**:
  - Relocated all backend-neutral domain models, search parameter parsers, compartment definitions, and `proto2type`-generated search index extractors out of `store/postgres` into `store/` and `gen/go/searchidx/`, eliminating cross-backend package imports from `server/`, `store/sqlite/`, and `store/doris/`.
- Synchronized runtime version reporting (`0.4.0`) across `flintd --version`, `CapabilityStatement.software.version`, OpenAPI 3.1.0 `info.version`, `Dockerfile`, `analytics/pyproject.toml`, `README.md`, and `site/`.

### Fixed

- Enforced mean systolic (`96608-5`) and mean diastolic (`96609-3`) LOINC component code validation on `us-core-average-blood-pressure` `Observation` resources.

## [0.3.0] - 2026-11-11

### Added

- **Decoupled FHIR R4 API Layer & Pluggable Storage Architecture (`store/`)**:
  - Cleanly separated the storage-agnostic FHIR R4 HTTP/clinical API layer (`server/`, `pkg/*`) from physical persistence via [`store.Store`](store/store.go) (operational CRUD/Search/History/Audit/OAuth2/Export contract), [`store.AnalyticalStore`](store/store.go) (columnar OLAP pushdown contract), and [`store.HybridStore`](store/hybrid.go) (real-time dual-plane OLTP→OLAP replication and `POST /fhir/r4/{tenant}/$sync-analytics` historical backfill).
  - **Embedded SQLite WAL Backend (`store/sqlite`)**: Pure-Go (`CGO_ENABLED=0`), zero-external-dependency operational and analytical backend (`--store=sqlite`, `--sqlite-dsn=flint.db` or `:memory:`) with 100% API and certification parity.
  - **Apache Doris MPP Backend (`store/doris`)**: Cloud-native Unique Key Merge-on-Write (MoW) columnar + `ARRAY<VARCHAR>` inverted-index backend with 13 zero-blob US Core resource tables (`VARBINARY` Proto3 + typed columns), runnable standalone (`--store=doris`) or paired as an OLAP replica (`--store=postgres --analytics-store=doris`).
  - **Hybrid Operational + Analytical Mode (`--analytics-store=doris|sqlite`)**: Sub-10ms ACID transactions on PostgreSQL paired with real-time replicated OLAP pushdown for `Observation/$stats`, Bulk Data `$export`, and population cohort counts.
- **Zero-JSON Binary Protobuf HTTP Wire Transport**:
  - End-to-end `application/x-protobuf` and `application/fhir+protobuf` (`google.fhir.r4.core.*`) content negotiation across CRUD, Search `Bundle`, `batch`/`transaction` `Bundle`, Conditional operations, and `PATCH`, plus binary load-testing support (`cmd/loadtest -wire=proto`).
- **HL7 SQL-on-FHIR v2 Engine (`ViewDefinition`)**:
  - In-memory `protoreflect` tabular runner (`ViewDefinition/$run` supporting `json`, `ndjson`, `csv`, and `tsv` output formats) and Bulk Data v2 export integration (`ViewDefinition/$export`).
  - Zero-JSON multi-dialect SQL compiler (`ViewDefinition/$sql?dialect=postgres|sqlite|doris`) supporting `select`, `column`, nested `forEach` / `forEachOrNull` (including scalar unnesting over FHIRPath `split()` and `toChars()`), `unionAll`, `where`, `constant`, `ofType()`, `extension()`, `getResourceKey()`, and `getReferenceKey()`.
- **35+ National & Specialty HL7 Implementation Guides & 130+ Clinical Operations**:
  - Expanded built-in IG and clinical operations coverage across US Core v6.1.0 & v7.0.0 (USCDI v3 & v4), ONC `(g)(10)` & HTI-1 `(b)(11)`, SMART App Launch v2.0.0 & Backend Services (`private_key_jwt`), UDAP Dynamic Registration, Da Vinci (HRex, PDex, CRD, DTR, PAS, CDex, ATR, DEQM, PCT, PCDE, Alerts, Risk Adjustment), CARIN Blue Button, ONC FAST NDH, Gravity SDOH, CodeX mCODE, PACIO, Vulcan RWD/eSource/ePI, Public Health (eCR, ELR, IIS, SANER, NHSN), HL7 IPS (`$summary`), HL7 IPA, Subscriptions Backport (`$status`, `$events`), HL7 SDC (`$populate`, `$extract`), Clinical Reasoning (`PlanDefinition/$apply`, `ActivityDefinition/$apply`, `Library/$evaluate`), CDS Hooks v2.0, and `TestScript/$evaluate`.
- **137-Check Automated Conformance & Certification Suite (`$certify`) & Upstream HL7 Test Harnesses**:
  - Expanded [`pkg/certify`](pkg/certify/certify.go), `cmd/certify`, and `GET|POST /fhir/r4/{tenant}/$certify` to **137 checks across 10 conformance suites**, verified identically across PostgreSQL, SQLite WAL, Apache Doris, and HybridStore topologies.
  - Wired all **22 official `FHIR/sql-on-fhir-v2` JSON test suites (144/144 tests)** against `ViewDefinition/$run` and verified live `ViewDefinition/$sql` compiled SQL execution parity on SQLite (`modernc.org/sqlite`) and PostgreSQL.
  - Wired the full **HL7 FHIRPath R4 `tests-fhir-r4.xml` specification corpus (1,050+ runtime vectors)** and **Synthea R4 US Core multi-patient `Bundle` parity** across all 5 runtime topologies (`postgres`, file-backed `sqlite` WAL, `doris`, `hybrid-postgres+doris`, and `hybrid-postgres+sqlite`).
  - Added headless **HL7 Inferno Conformance CI** ([`.github/workflows/inferno.yml`](.github/workflows/inferno.yml), [`deploy/inferno/headless/`](deploy/inferno/headless/)) executing official `us_core_test_kit` (`v6.1.0`), `smart_app_launch_test_kit` (`STU2` Backend Services `ES384` `private_key_jwt` + RFC 7591 `/oauth2/register`), and `bulk_data_test_kit` (`v2.0.0`) against live `flintd` on both PostgreSQL and SQLite.
- **Architecture Documentation**:
  - Added [`docs/architecture.md`](docs/architecture.md) detailing the strict architectural boundary between Flint's FHIR R4 API layer and its pluggable storage & analytical plane.

### Fixed

- Aligned `Taskfile.yaml` `run-sqlite` environment variables (`FLINT_STORE_BACKEND=sqlite FLINT_SQLITE_DSN=flint.db`) and `README.md` configuration tables with `cmd/flintd` CLI/env resolution.
- Synchronized runtime version reporting (`0.3.0`) across `flintd --version`, `CapabilityStatement.software.version` (`GET /fhir/r4/metadata`), OpenAPI 3.1.0 `info.version` (`GET /fhir/r4/openapi.json`), `Dockerfile`, and `analytics/pyproject.toml`.

## [0.2.0] - 2026-04-06

### Added

- **Physical Schema-per-Tenant Isolation (`--tenant-isolation=schema`)**:
  - Dedicated `tenant_<slug>` PostgreSQL schemas isolating `fhir_resources`, `_history`, `spidx_*` search indexes, `AuditEvent` logs, and Bulk `$export` jobs per tenant, alongside shared-table PostgreSQL RLS mode (`--tenant-isolation=shared`) and per-transaction role switching (`FLINT_TENANT_ROLE_SWITCH`).

## [0.1.0] - 2026-03-31

### Added

- Initial open-source release of **Flint FHIR R4 Server**:
  - `google/fhir` R4 Protocol Buffer canonical persistence (`BYTEA`, zero JSONB) and 6 HAPI-style covering `spidx_*` B-tree search tables.
  - Core FHIR R4 CRUD, versioning (`ETag` / `If-Match`), `Bundle` (`batch` / `transaction`), SMART on FHIR v2 JWT auth, ASTM E2147 `AuditEvent` logging, Bulk Data `$export`, OpenAPI 3.1.0 & `$docs` sandbox, and optional AutoMQ → Apache Iceberg → SQLMesh CDC lakehouse add-ons.
