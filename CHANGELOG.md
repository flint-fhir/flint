# Changelog

All notable changes to **Flint** will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
- **137-Check Automated Conformance & Certification Suite (`$certify`)**:
  - Expanded [`pkg/certify`](pkg/certify/certify.go), `cmd/certify`, and `GET|POST /fhir/r4/{tenant}/$certify` to **137 checks across 10 conformance suites**, verified identically across PostgreSQL, SQLite WAL, Apache Doris, and HybridStore topologies.
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
