# Changelog

All notable changes to **Flint** will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
