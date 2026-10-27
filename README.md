# Flint — High-Performance Open Source FHIR R4 Server

[![CI](https://github.com/flint-fhir/flint/actions/workflows/ci.yml/badge.svg)](https://github.com/flint-fhir/flint/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go)](go.mod)
[![FHIR R4](https://img.shields.io/badge/FHIR-R4.0.1-firebrick)](https://hl7.org/fhir/R4/)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Code of Conduct](https://img.shields.io/badge/Contributor%20Covenant-2.1-4baaaa.svg)](CODE_OF_CONDUCT.md)

**Flint** is an open-source, cloud-agnostic **FHIR R4 server** built in Go on PostgreSQL. It provides high-throughput transactional EHR persistence, sub-10ms covering-index search, SMART on FHIR v2 authorization, US Core v6.1.0 & v7.0.0 conformance, and ONC `(g)(10)` Bulk Data capabilities.

All domain models, search index extractors, and storage structures are compiled directly from `google/fhir` Protocol Buffers using [proto2type](https://github.com/protocgen/proto2type).

---

## Why Flint?

Traditional FHIR servers (e.g. HAPI FHIR, Medplum) often incur heavy runtime overhead and complex schema maintenance:

| Dimension | Traditional FHIR Servers | Flint |
|:----------|:-------------------------|:------|
| **Data Format** | Heavy JSON / relational table sprawl | Compact Protocol Buffers (**16× wire reduction**, 124B vs ~2KB) |
| **Search Indexing** | Deep relational joins or Elasticsearch sync | **6 static covering index tables** per tenant (HAPI pattern), sub-10ms queries |
| **Schema Maintenance** | Manual migrations across hundreds of FHIR types | **100% generated** via `proto2type` from official HL7 SearchParameter JSON |
| **Multi-Tenancy** | Soft tenancy (`tenant_id` column filters) | **Schema-per-tenant physical isolation** for HIPAA compliance |
| **Throughput** | Bottlenecked by JVM garbage collection | **>1,400 resources/sec** transactional store, **>1,000 resources/sec** HTTP |

---

## Architecture

```mermaid
flowchart TD
    subgraph Clients ["Interoperability Clients"]
        EHR["EHR / SMART Apps / Synthea"] -->|FHIR R4 Resources & Bundles| API["Flint HTTP API (flintd)"]
    end

    subgraph Server ["Flint FHIR R4 Server"]
        API --> Auth["SMART on FHIR v2 Auth & Scope Enforcer"]
        Auth --> Val["StructureDefinition & ValueSet Validator"]
        Val --> Engine["REST / Bundle / Terminology / CDS Hooks / eCQM Engine"]
        Engine -->|rest-hook Webhooks| Sub["FHIR R4 Subscriptions (rest-hook)"]
    end

    subgraph OLTP ["Operational Plane (PostgreSQL)"]
        Engine -->|ACID Transactions| PG["PostgreSQL (Schema-per-Tenant)"]
        PG --> RProto["fhir_resource & fhir_resource_history (Proto Blobs)"]
        PG --> Spidx["6x Covering spidx_* Tables\n(string, token, date, ref, quantity, uri)"]
        PG --> Audit["fhir_audit_event & OAuth2 / $export State"]
    end
```

---

## Verified Benchmarks

Flint includes a dedicated benchmarking harness ([`cmd/loadtest`](cmd/loadtest/)) to measure ingestion throughput, database scaling, and latency percentiles on local and cloud environments:

* **Direct Postgres Store Ingestion**: **1,410+ resources/sec** (282+ bundles/sec) sustained with transactional ACID upserts and index extraction across Big 5 resources.
* **HTTP REST API Transaction Bundles**: **1,024+ resources/sec** (205+ bundles/sec) sustained with 100 concurrent workers and **100% 2xx success rate**.
* **Latency Profile**:
  * $p50 < 250\text{ms}$
  * $p90 < 360\text{ms}$
  * $p99 < 500\text{ms}$

---

## Supported FHIR Resources (All 146 FHIR R4 Types & 26 US Core v6.1.0 / v7.0.0 Profiles) & Conformance Matrix

Flint implements full search index extraction, transactional persistence, SMART on FHIR v2 compartment isolation, granular sub-resource scopes (`?category=`) & **Backend Services asymmetric `private_key_jwt` (`RS256`/`RS384`/`ES384`, RFC 7523)**, **HL7 FHIRPath Patch (`Parameters`) / RFC 6902 JSON Patch / RFC 7396 Merge Patch**, **Conditional Read (`If-None-Match` & `If-Modified-Since` → `304 Not Modified` & `HEAD`), Conditional Create/Update/Patch/Delete (Single & Multiple `_count`), `If-Match` Optimistic Locking on `PUT`/`PATCH`/`DELETE` (`412 Precondition Failed`) & `Prefer: handling=strict|lenient` (`Preference-Applied` & `OperationOutcome` warnings)**, **All 5 FHIR R4 Compartments (`Patient`, `Encounter`, `Practitioner`, `RelatedPerson`, `Device`) including Wildcard Multi-Type `/{Compartment}/{id}/*` Search, `Patient/$everything` & `Encounter/$everything` (GET & POST with `_type`/`_typeFilter`/`_since`/`_count`/`_offset` pagination) & `Patient/{id}/$summary` (HL7 IPS)**, **`DocumentReference/$docref` (US Core / Argonaut Clinical Notes + On-Demand IPS)**, **`X-Provenance` Header & `/{resourceType}/{id}/$provenance` Lineage**, **`Composition/{id}/$document` (`persist=true` document `Bundle` storage)**, **`Questionnaire/$populate` & `QuestionnaireResponse/$extract` (HL7 Da Vinci SDC + Auto-Provenance)**, **`Patient/$match` (MPI) & `Patient/$member-match` (Da Vinci HRex/PDex)**, **`Claim/$submit` (Da Vinci PAS)**, **Token `:of-type` Modifier (`?identifier:of-type=typeSystem|typeCode|value`), Terminology-Aware Search Modifiers (`:in`, `:not-in`, `:below`, `:above`), Reference `:identifier` Modifier (`?patient:identifier=system|value`), Composite Search (`code-value-quantity`, `component-code-value-quantity`, `combo-code-value-quantity`, `code-value-concept`, `component-code-value-concept`, `combo-code-value-concept`, `code-value-date`, `code-value-string`), Date `ap` (Approximately) Prefix, Wildcard `_include=*` / `_revinclude=*`, Multi-Hop Recursive `_has` Reverse Chaining, `_list` Cohort Search & Enhanced Forward Chaining (`:exact`, `:not`, reference)**, **Full `_history` Conformance (`_since`, `_at`, `_list`, `_summary`, `_elements`, `Bundle.link`, `response.location`) & System/Type/Instance `$meta` / `$meta-add` / `$meta-delete`**, **Clinical Quality Measures (`Measure/$evaluate-measure` & `Measure/$care-gaps` — CMS165 / CMS122 eCQM / Da Vinci DEQM)**, **HL7 CDS Hooks v2.0 (`/cds-services`)**, **Terminology Services (`$expand`, `$validate-code`, `$lookup`, `CodeSystem/$subsumes`, `ConceptMap/$translate`)**, **`Subscription` (`rest-hook`)**, and **Proto-compiled OpenAPI 3.1.0 schemas** (`protoreflect.MessageDescriptor` → OpenAPI 3.1 JSON Schema) across **all 146 FHIR R4 resource types** and **all 26 US Core v6.1.0 / v7.0.0 (USCDI v3 & v4) profiles**:

| Resource / Operation | US Core v6.1.0 / v7.0.0 / ONC `(g)(10)` Profile | Interactions | Search Parameters (`spidx_*`) | SMART v2 Scopes | Status |
|:---------------------|:------------------------------------------------|:-------------|:------------------------------|:----------------|:-------|
| **`Patient`** | US Core Patient (`v6.1.0` & `v7.0.0`), HL7 IPS (`$summary`), MPI (`$match`), Da Vinci HRex (`$member-match`) | `read`, `vread`, `update`, `patch`, `delete`, `history-instance`, `history-type`, `create`, `search-type`, `compartment-search`, `conditional-create/update/delete`, `$everything` (`GET`/`POST`), `$summary`, `$match`, `$member-match`, `$meta`, `$meta-add`, `$meta-delete`, `$export`, `$purge`, `$expunge`, `$diff`, `$validate` | `_id`, `_lastUpdated`, `identifier` (`:of-type`), `name`, `family`, `given`, `birthdate` (`ap`), `gender`, `active`, `address-city`, `address-state`, `address-postalcode`, `organization`, `_has` (multi-hop), `_list`, `_revinclude` (`*`) | `patient/Patient.rs`, `user/Patient.cruds`, `system/Patient.rs` | ✅ Production Ready |
| **`Encounter`** | US Core Encounter (`v6.1.0` & `v7.0.0`) | `read`, `vread`, `update`, `patch`, `delete`, `history-instance`, `history-type`, `create`, `search-type`, `conditional-create/update/delete`, `$validate` | `_id`, `_lastUpdated`, `identifier` (`:of-type`), `date` (`ap`), `patient`, `subject`, `status`, `class`, `type`, `location`, `_include` (`*`) | `patient/Encounter.rs`, `user/Encounter.cruds`, `system/Encounter.rs` | ✅ Production Ready |
| **`Condition`** | US Core Condition Problems/Health Concerns & Encounter Diagnosis (`v6.1.0` & `v7.0.0`) | `read`, `vread`, `update`, `patch`, `delete`, `history-instance`, `history-type`, `create`, `search-type`, `conditional-create/update/delete`, `$validate` | `_id`, `_lastUpdated`, `patient`, `subject`, `code`, `clinical-status`, `verification-status`, `category`, `onset-date`, `recorded-date`, `encounter`, `_include` (`*`) | `patient/Condition.rs`, `user/Condition.cruds`, `system/Condition.rs` | ✅ Production Ready |
| **`Observation`** | US Core Vital Signs, Lab, Blood Pressure, Avg BP, Care/Treatment Preference, Simple & Screening (`v6.1.0` & `v7.0.0`) | `read`, `vread`, `update`, `patch`, `delete`, `history-instance`, `history-type`, `create`, `search-type`, `conditional-create/update/delete`, `$validate` | `_id`, `_lastUpdated`, `patient`, `subject`, `code`, `category`, `status`, `date` (`ap`), `value-quantity`, `value-concept`, `value-date`, `value-string`, `component-code`, `combo-code`, `component-value-quantity`, `combo-value-quantity`, `component-value-concept`, `combo-value-concept`, `code-value-quantity`, `component-code-value-quantity`, `combo-code-value-quantity`, `code-value-concept`, `component-code-value-concept`, `combo-code-value-concept`, `code-value-date`, `code-value-string`, `_include` (`*`) | `patient/Observation.rs` (incl. `?category=`), `user/Observation.cruds`, `system/Observation.rs` | ✅ Production Ready |
| **`MedicationRequest`** | US Core MedicationRequest (`v6.1.0` & `v7.0.0`) | `read`, `vread`, `update`, `patch`, `delete`, `history-instance`, `history-type`, `create`, `search-type`, `conditional-create/update/delete`, `$validate` | `_id`, `_lastUpdated`, `patient`, `subject`, `status`, `intent`, `code`, `authoredon`, `encounter`, `requester`, `medication`, `_include` | `patient/MedicationRequest.rs`, `user/MedicationRequest.cruds`, `system/MedicationRequest.rs` | ✅ Production Ready |
| **`AllergyIntolerance`** | US Core AllergyIntolerance (`v6.1.0` & `v7.0.0`) | `read`, `vread`, `update`, `patch`, `delete`, `history-instance`, `history-type`, `create`, `search-type`, `conditional-create/update/delete`, `$validate` | `_id`, `_lastUpdated`, `patient`, `subject`, `clinical-status`, `verification-status`, `code`, `criticality`, `date`, `_include` | `patient/AllergyIntolerance.rs`, `user/AllergyIntolerance.cruds`, `system/AllergyIntolerance.rs` | ✅ Production Ready |
| **`Procedure`** | US Core Procedure (`v6.1.0` & `v7.0.0`) | `read`, `vread`, `update`, `patch`, `delete`, `history-instance`, `history-type`, `create`, `search-type`, `conditional-create/update/delete`, `$validate` | `_id`, `_lastUpdated`, `patient`, `subject`, `status`, `code`, `date`, `encounter`, `performer`, `_include` | `patient/Procedure.rs`, `user/Procedure.cruds`, `system/Procedure.rs` | ✅ Production Ready |
| **`DiagnosticReport`** | US Core DiagnosticReport Lab & Note (`v6.1.0` & `v7.0.0`) | `read`, `vread`, `update`, `patch`, `delete`, `history-instance`, `history-type`, `create`, `search-type`, `conditional-create/update/delete`, `$validate` | `_id`, `_lastUpdated`, `patient`, `subject`, `status`, `category`, `code`, `date`, `issued`, `result`, `_include` | `patient/DiagnosticReport.rs`, `user/DiagnosticReport.cruds`, `system/DiagnosticReport.rs` | ✅ Production Ready |
| **`Immunization`** | US Core Immunization (`v6.1.0` & `v7.0.0`) | `read`, `vread`, `update`, `patch`, `delete`, `history-instance`, `history-type`, `create`, `search-type`, `conditional-create/update/delete`, `$validate` | `_id`, `_lastUpdated`, `patient`, `subject`, `status`, `vaccine-code`, `date`, `lot-number`, `_include` | `patient/Immunization.rs`, `user/Immunization.cruds`, `system/Immunization.rs` | ✅ Production Ready |
| **`Specimen` & `Location`** | US Core Specimen & Location (`v7.0.0` / USCDI v4) | Full CRUD, `patch`, `vread`, `_history`, `search-type`, `conditional-*`, `$validate` | `_id`, `_lastUpdated`, `identifier`, `patient`, `subject`, `type`, `status`, `bodysite`, `collected`, `name`, `address`, `address-city`, `address-state`, `address-postalcode`, `organization` | `patient/Specimen.rs`, `user/Location.rs`, `system/*.rs` | ✅ Production Ready |
| **`DocumentReference` & `Provenance`** | US Core DocumentReference, Clinical Notes (`$docref`) & Provenance (`v6.1.0` & `v7.0.0`) | Full CRUD, `patch`, `vread`, `_history`, `search-type`, `conditional-*`, `DocumentReference/$docref` (`GET`/`POST`), `X-Provenance` header, `/{resourceType}/{id}/$provenance`, `$validate` | `_id`, `_lastUpdated`, `identifier`, `patient`, `subject`, `status`, `type`, `category`, `date`, `period`, `author`, `target`, `agent`, `recorded`, `_revinclude` | `patient/DocumentReference.rs`, `patient/Provenance.rs`, `system/*.rs` | ✅ Production Ready |
| **Care Coordination & Coverage (`CarePlan`, `CareTeam`, `Goal`, `ServiceRequest`, `Coverage`, `QuestionnaireResponse`, `MedicationDispense`, `Medication`, `Device`, `Practitioner`, `PractitionerRole`, `Organization`, `RelatedPerson`)** | US Core `v6.1.0` & `v7.0.0` | Full CRUD, `patch`, `vread`, `_history`, `search-type`, `conditional-*`, `$validate` | `_id`, `_lastUpdated`, `identifier`, `patient`, `subject`, `status`, `category`, `code`, `date`, `intent`, `lifecycle-status`, `target-date`, `authored`, `questionnaire`, `whenhandedover`, `type`, `udi-di`, `practitioner`, `organization`, `specialty`, `role`, `name`, `address-*` | `patient/*.rs`, `user/*.cruds`, `system/*.rs` | ✅ Production Ready |
| **All 146 FHIR R4 Resource Types** | HL7 FHIR R4.0.1 Base Specification | Full CRUD, `patch`, `vread`, `_history`, `search-type`, `search-system` (`?_type=`), `conditional-*`, `$meta`, `$meta-add`, `$meta-delete`, `$diff`, `$graph`, `$expunge`, `$validate`, OpenAPI 3.1 | Dynamic `protoreflect` index extraction across `spidx_string` (including `:text`), `spidx_token`, `spidx_date`, `spidx_reference`, `spidx_quantity`, `spidx_uri` | `patient/*.rs`, `user/*.cruds`, `system/*.rs` | ✅ Production Ready |
| **`Questionnaire`, `QuestionnaireResponse` & `Composition` (HL7 SDC & Document)** | HL7 Da Vinci DTR / Structured Data Capture (SDC) & Clinical Document Assembly | `GET/POST /Questionnaire/$populate`, `/Questionnaire/{id}/$populate`, `POST /QuestionnaireResponse/$extract`, `/QuestionnaireResponse/{id}/$extract` (with automatic `Provenance` lineage), `GET/POST /Composition/{id}/$document` | `subject`, `identifier`, `persist` | `user/Questionnaire.rs`, `user/QuestionnaireResponse.cruds`, `patient/*.rs` | ✅ Production Ready |
| **`Claim` & `Claim/$submit` (Da Vinci PAS)** | HL7 Da Vinci Prior Authorization Support (PAS) & FHIR R4 Financial | `POST /Claim/$submit` (accepts `Claim`, `Bundle`, or `Parameters`; persists & returns adjudicated `ClaimResponse` with `preAuthRef`) | `_id`, `patient`, `provider`, `status`, `use`, `created` | `user/Claim.cruds`, `system/Claim.cruds`, `patient/Claim.rs` | ✅ Production Ready |
| **`Measure` & `$evaluate-measure` (eCQM)** | HL7 FHIR R4 Clinical Quality Measures / CMS eCQM (`CMS165`, `CMS122`) | `GET/POST /Measure/$evaluate-measure`, `/Measure/{id}/$evaluate-measure` (`MeasureReport` `individual` & `summary`) | `measure`, `periodStart`, `periodEnd`, `subject`, `reportType` | `user/Measure.rs`, `system/Measure.rs`, `patient/*.rs` | ✅ Production Ready |
| **HL7 CDS Hooks v2.0 (`/cds-services`)** | HL7 CDS Hooks v2.0 Clinical Decision Support (`patient-view`, `order-select`) | `GET /cds-services`, `POST /cds-services/{serviceId}` (auto-hydrating FHIR prefetch + actionable Cards/Suggestions) | `flint-hypertension-care-gap`, `flint-diabetes-a1c-care-gap`, `flint-medication-allergy-check` | Public Discovery / SMART Bearer Token | ✅ Production Ready |
| **Terminology (`ValueSet`, `CodeSystem`, `ConceptMap`)** | HL7 FHIR R4 Terminology Services & US Core ValueSets | `GET/POST ValueSet/$expand`, `ValueSet/{id}/$expand`, `ValueSet/$validate-code`, `ValueSet/{id}/$validate-code`, `CodeSystem/$lookup`, `CodeSystem/{id}/$lookup`, `ConceptMap/$translate`, `ConceptMap/{id}/$translate` | `url`, `filter`, `count`, `offset`, `code`, `system`, `targetsystem`, `reverse`, `display`, `coding` (`Parameters` or query) | `user/ValueSet.rs`, `system/ValueSet.rs`, `patient/*.rs` | ✅ Production Ready |
| **`Subscription` (`rest-hook` & `$status`)** | HL7 FHIR R4 Subscription Notification Framework & Backport `$status` | `POST/PUT/PATCH /Subscription` (`requested` → `active` lifecycle, `Subscription.end` auto-expiration), `GET/POST /Subscription/{id}/$status`, `/Subscription/$status`, real-time HTTP `POST` webhook delivery | Full search criteria evaluation (`date`, `quantity`, `token`, `:not`, `string` `:exact`/`:contains`, `reference` `:TargetType`, `uri` `:below`/`:above`, `composite`, `:missing`) | `user/Subscription.cruds`, `system/Subscription.cruds` | ✅ Production Ready |
| **`AuditEvent`** | ASTM E2147 / ONC `§170.315(d)(2)` Auditable Events | `read`, `search-type` | `_id`, `date`, `entity`, `patient`, `agent`, `action`, `subtype`, `outcome` | `user/AuditEvent.rs`, `system/AuditEvent.rs` | ✅ Production Ready |
| **`Bundle`** | HL7 FHIR R4 Batch & ACID Transaction | `batch`, `transaction` (`POST`, `PUT`, `PATCH`, `DELETE`, `GET`, `HEAD`, `urn:uuid:` resolution, `ifNoneExist`, `ifMatch`, `ifNoneMatch`, `ifModifiedSince`, conditional `?searchParams` URLs) | N/A | Per-entry SMART scope & compartment validation | ✅ Production Ready |
| **`$export` & `$import` (Bulk Data)** | HL7 FHIR Bulk Data Access v2.0.0 / ONC `§170.315(g)(10)` | `GET/POST` System (`/$export`), All-Patients (`/Patient/$export`), Active Group Member Cohort (`/Group/{id}/$export`), Single-Patient (`/Patient/{id}/$export`), Bulk NDJSON Import (`POST /$import`, `/$import-poll/{jobId}`), Postgres-persisted Async Poll (`/$export-poll/{jobId}`), NDJSON Download (`/$export-file/{jobId}/{file}`) | `_type`, `_since`, `_typeFilter`, `patient`, `_outputFormat`, `inputFormat`, `input` (`application/fhir+ndjson`) | `system/*.rs` (Export), `system/*.cruds` (Import), `patient/*.rs` (Single-Patient) | ✅ Production Ready |
| **`$certify` (Conformance)** | HL7 FHIR R4 `TestReport` / US Core `v6.1.0` & `v7.0.0` / ONC `(g)(10)` & HTI-1 | `GET/POST /fhir/r4/{tenant}/$certify` & CLI (`task certify`) | 50-check automated conformance harness across 5 suites | Public / Admin | ✅ Production Ready |

---

## Installation & Quickstart (Standalone FHIR R4 Server)

`flintd` is a **single, self-contained binary and OCI container** that embeds all SQL schema migrations and requires **only PostgreSQL 15+**.

### Option 1: Docker Compose (Fastest — Postgres + FHIR Server + Demo Cohort)

Run the standalone FHIR R4 server and PostgreSQL (and seed the 25-resource US Core `"demo"` cohort) with a single command — no Go, Nix, or Kubernetes required:

```bash
curl -fsSL https://raw.githubusercontent.com/flint-fhir/flint/main/docker-compose.yml -o docker-compose.yml
docker compose --profile seed up -d
```

*(Or from a cloned repository: `docker compose --profile seed up -d --build` or `task compose-up`)*

Once running, open:
* **Interactive SMART v2 Sandbox & Clinical Recipe Runner (`$docs`)**: [http://localhost:8080/fhir/r4/demo/$docs](http://localhost:8080/fhir/r4/demo/$docs)
* **Live 50-Check Conformance `TestReport` (`$certify`)**: [http://localhost:8080/fhir/r4/demo/$certify](http://localhost:8080/fhir/r4/demo/$certify)
* **Proto-Compiled OpenAPI 3.1.0 Spec**: [http://localhost:8080/fhir/r4/demo/openapi.json](http://localhost:8080/fhir/r4/demo/openapi.json)

### Option 2: Container Image (`ghcr.io/flint-fhir/flint`)

Run the prebuilt multi-arch container (`linux/amd64`, `linux/arm64`) against any existing PostgreSQL instance. SQL migrations are applied automatically on startup:

```bash
docker run --rm -p 8080:8080 \
  -e FLINT_POSTGRES_DSN="postgres://flint:flint@host.docker.internal:5432/flint?sslmode=disable" \
  ghcr.io/flint-fhir/flint:latest
```

Seed the US Core `"demo"` cohort or run the 50-check certification CLI using the same container image:

```bash
# Seed the 25-resource US Core v6.1.0 & v7.0.0 cohort into tenant "demo"
docker run --rm --entrypoint /usr/local/bin/seed \
  ghcr.io/flint-fhir/flint:latest -url http://host.docker.internal:8080 -tenant demo

# Run the 50-check conformance certification suite
docker run --rm --entrypoint /usr/local/bin/certify \
  ghcr.io/flint-fhir/flint:latest -url http://host.docker.internal:8080 -tenant demo
```

### Option 3: Single Binary (`go install` or GitHub Releases)

Download a prebuilt archive from [GitHub Releases](https://github.com/flint-fhir/flint/releases) or install directly with Go 1.25+:

```bash
go install github.com/flint-fhir/flint/cmd/flintd@latest

# Run against any PostgreSQL database (auto-migrates on boot)
FLINT_POSTGRES_DSN="postgres://flint:flint@localhost:5432/flint?sslmode=disable" \
FLINT_ADDR=":8080" \
  flintd
```

### Runtime Configuration (`flintd`)

| Environment Variable | Default | Required | Description |
|:---------------------|:--------|:---------|:------------|
| `FLINT_POSTGRES_DSN` | `postgres://flint:flint@localhost:5432/flint?sslmode=disable` | **Yes** | PostgreSQL connection string (auto-runs embedded migrations on boot) |
| `FLINT_ADDR` | `:8080` | No | HTTP listen address |
| `FLINT_OIDC_ISSUER` | *(empty — dev mode)* | No | Enable external OIDC / SMART v2 token validation |
| `FLINT_OIDC_AUDIENCE` | *(empty)* | No | Expected JWT audience claim |
| `FLINT_AUTH_PROVIDER` | `oidc` (`oidc` \| `zitadel`) | No | OIDC identity provider adapter |
| `FLINT_AUTOMQ_BROKERS` | *(empty — disabled)* | No | **[Add-On]** Comma-separated Kafka/AutoMQ brokers for CDC streaming |

---

### Option 4: Contributor Development Environment (Nix + `k3d` Kubernetes)

Flint also provides a hermetic contributor environment with Nix, direnv, and `k3d`:

```bash
# Clone the repository & enter hermetic Nix shell
git clone https://github.com/flint-fhir/flint.git
cd flint
direnv allow # or: nix develop

# Option A: 1-command Kubernetes deployment (k3d + Postgres + flintd + demo cohort)
task k3d-up

# Option B: Local binary development against k3d Postgres
task dev
task build
task run
task seed
task certify
```

### Run Benchmarks & Tests

```bash
# Run Go unit, property, and integration tests
task test

# Run automated US Core v6.1.0 & v7.0.0 / ONC (g)(10) conformance certification suite
task certify

# Run high-throughput HTTP load test
task bench-http

# Run direct Postgres store load test
task bench-store
```

---

## Copy-Pasteable API Cookbook (`curl`)

After running `task run` and `task seed`, you can explore the seeded `"demo"` tenant (`2 Practitioners`, `3 Patients`, `3 Encounters`, `2 Conditions`, `5 Observations`) directly from your terminal:

```bash
# 1. Assemble Alice Wonderland's complete longitudinal chart ($everything)
curl -s "http://localhost:8080/fhir/r4/demo/Patient/pat-alice-001/\$everything" | jq .

# 2. Search heart rate Observations > 70 bpm and include the referenced Patient
curl -s "http://localhost:8080/fhir/r4/demo/Observation?code=8867-4&value-quantity=gt70&_include=Observation:patient" | jq .

# 3. Chained search: find all Observations for patients with family name "Wonderland"
curl -s "http://localhost:8080/fhir/r4/demo/Observation?patient.family=Wonderland" | jq .

# 4. Date range search: find Encounters between 2026-01-01 and 2026-02-01
curl -s "http://localhost:8080/fhir/r4/demo/Encounter?date=ge2026-01-01&date=le2026-02-01" | jq .

# 5. Conditional Create (If-None-Exist): idempotently return existing Patient if MRN-1001 exists
curl -i -X POST "http://localhost:8080/fhir/r4/demo/Patient" \
  -H "Content-Type: application/fhir+json" \
  -H "If-None-Exist: identifier=http://hospital.example.org/mrn|MRN-1001" \
  -d '{"resourceType":"Patient","name":[{"family":"Wonderland","given":["Alice"]}]}'

# 6. Optimistic concurrency control (If-Match) + Version History (_history)
curl -i -X PUT "http://localhost:8080/fhir/r4/demo/Patient/pat-alice-001" \
  -H "Content-Type: application/fhir+json" \
  -H 'If-Match: W/"1"' \
  -d '{"resourceType":"Patient","id":"pat-alice-001","active":true,"gender":"female","birthDate":"1985-04-12","name":[{"family":"Wonderland","given":["Alice","Marie"]}]}'

curl -s "http://localhost:8080/fhir/r4/demo/Patient/pat-alice-001/_history" | jq .

# 7. Transaction Bundle with intra-bundle urn:uuid: reference resolution
curl -s -X POST "http://localhost:8080/fhir/r4/demo" \
  -H "Content-Type: application/fhir+json" \
  -d '{
    "resourceType": "Bundle",
    "type": "transaction",
    "entry": [
      {
        "fullUrl": "urn:uuid:11111111-2222-3333-4444-555555555555",
        "resource": {"resourceType": "Patient", "name": [{"family": "Curie", "given": ["Marie"]}], "gender": "female"},
        "request": {"method": "POST", "url": "Patient"}
      },
      {
        "fullUrl": "urn:uuid:66666666-7777-8888-9999-000000000000",
        "resource": {
          "resourceType": "Observation",
          "status": "final",
          "code": {"coding": [{"system": "http://loinc.org", "code": "8867-4"}]},
          "subject": {"reference": "urn:uuid:11111111-2222-3333-4444-555555555555"},
          "valueQuantity": {"value": 68, "unit": "/min", "system": "http://unitsofmeasure.org", "code": "/min"}
        },
        "request": {"method": "POST", "url": "Observation"}
      }
    ]
  }' | jq .

# 8. Validate a FHIR R4 resource without persisting ($validate)
curl -s -X POST "http://localhost:8080/fhir/r4/demo/Patient/\$validate" \
  -H "Content-Type: application/fhir+json" \
  -d '{"resourceType":"Patient","gender":"female","birthDate":"1990-05-15","name":[{"family":"Doe"}]}' | jq .

# 9. Query HIPAA / ONC Security AuditEvents
curl -s "http://localhost:8080/fhir/r4/demo/AuditEvent?patient=pat-alice-001&_count=5" | jq .

# 10. Bulk Data Access ($export) — Direct NDJSON Streaming (ONC (g)(10))
curl -s "http://localhost:8080/fhir/r4/demo/Patient/pat-alice-001/\$export?_type=Patient,Observation" \
  -H "Accept: application/fhir+ndjson"

# 11. Bulk Data Access ($export) — Async Kick-Off (202 Accepted) + Job Status Poll + NDJSON File Download
curl -i -s "http://localhost:8080/fhir/r4/demo/\$export?_type=Patient,Condition,Observation" \
  -H "Prefer: respond-async" \
  -H "Accept: application/fhir+json"

# 12. Inspect the Protobuf-compiled OpenAPI 3.1.0 specification
curl -s "http://localhost:8080/fhir/r4/demo/openapi.json" | jq '.components.schemas.Patient | {title, proto: ."x-protobuf-full-name", fields: (.properties | keys)}'
```

---

## API Reference

Flint implements standard FHIR R4 REST API interactions:

### Batch / Transaction Bundles
```http
POST /fhir/r4/{tenant}
Content-Type: application/fhir+json

{
  "resourceType": "Bundle",
  "type": "transaction",
  "entry": [
    {
      "resource": {
        "resourceType": "Patient",
        "id": "pat-123",
        "name": [{"family": "Smith", "given": ["Jane"]}],
        "gender": "female"
      },
      "request": { "method": "PUT", "url": "Patient/pat-123" }
    }
  ]
}
```

### Resource Interactions
* **Point Read**: `GET /fhir/r4/{tenant}/{resourceType}/{id}` (supports `_summary=true|text|data` and `_elements=...` with `SUBSETTED` tag; returns `ETag: W/"<version>"` and `Last-Modified`; `410 Gone` if deleted)
* **Version Read (`vread`)**: `GET /fhir/r4/{tenant}/{resourceType}/{id}/_history/{vid}` (supports `_summary=true|text|data` and `_elements=...` projection)
* **History (`_history`)**: Instance (`GET /fhir/r4/{tenant}/{resourceType}/{id}/_history`), Type (`GET /fhir/r4/{tenant}/{resourceType}/_history`), and System (`GET /fhir/r4/{tenant}/_history`) with `_since`, `_at` (point-in-time/period currency and comparators), `_list`, `_summary` (`count`, `true`, `text`, `data`), `_elements`, `_count`, `_offset`, `Bundle.link` (`self`/`next`/`previous`), and `entry.response.location`
* **Create & Conditional Create (`If-None-Exist`)**: `POST /fhir/r4/{tenant}/{resourceType}` (`201 Created` + `Location` + `ETag`; when `If-None-Exist` header or Bundle `request.ifNoneExist` matches 1 existing resource, returns `200 OK` idempotently; returns `412 Precondition Failed` on multiple matches)
* **Update / Create-on-Update & Conditional Update**: `PUT /fhir/r4/{tenant}/{resourceType}/{id}` and `PUT /fhir/r4/{tenant}/{resourceType}?{searchParams}` (supports optimistic concurrency via `If-Match: W/"<version>"`, returning `412 Precondition Failed` on version conflict)
* **FHIRPath Patch, JSON Patch, Merge Patch & Conditional Patch**: `PATCH /fhir/r4/{tenant}/{resourceType}/{id}` and `PATCH /fhir/r4/{tenant}/{resourceType}?{searchParams}` (`application/fhir+json` HL7 FHIRPath Patch `Parameters`, `application/json-patch+json` RFC 6902, and `application/merge-patch+json` RFC 7396)
* **Soft Delete & Conditional Delete (Single & Multiple `_count`)**: `DELETE /fhir/r4/{tenant}/{resourceType}/{id}` and `DELETE /fhir/r4/{tenant}/{resourceType}?{searchParams}` (`204 No Content` + tombstone version `ETag`; supports `If-Match: W/"<version>"` optimistic concurrency returning `412 Precondition Failed` on version conflict, `_count` for multiple conditional deletion, and `Prefer: return=OperationOutcome`)
* **All 5 FHIR R4 Compartments (`Patient`, `Encounter`, `Practitioner`, `RelatedPerson`, `Device`, Wildcard `/*` & `$everything`)**: `GET/POST /fhir/r4/{tenant}/Patient/{id}/$everything`, `GET/POST /fhir/r4/{tenant}/Encounter/{id}/$everything` (with `_type`, `_typeFilter`, `_since`, `_count`, `_offset`), `GET/POST /fhir/r4/{tenant}/{compartmentType}/{id}/{resourceType}` (including `POST .../_search`), and Wildcard Multi-Type `GET/POST /fhir/r4/{tenant}/{compartmentType}/{id}/*`
* **Terminology Services (`$expand`, `$validate-code`, `$lookup`)**:
  * `GET/POST /fhir/r4/{tenant}/ValueSet/$expand` & `/ValueSet/{id}/$expand`
  * `GET/POST /fhir/r4/{tenant}/ValueSet/$validate-code` & `/ValueSet/{id}/$validate-code`
  * `GET/POST /fhir/r4/{tenant}/CodeSystem/$lookup` & `/CodeSystem/{id}/$lookup`
* **Subscriptions (`rest-hook`)**: `POST/PUT /fhir/r4/{tenant}/Subscription` (automatically activates `"requested"` → `"active"` and delivers HTTP `POST` webhooks on matching resource writes)
* **Bulk Data Access (`$export` — ONC `(g)(10)`)**:
  * System-level: `GET /fhir/r4/{tenant}/$export?_type=Patient,Observation&_since=2025-01-01T00:00:00Z`
  * All-Patients Compartment: `GET /fhir/r4/{tenant}/Patient/$export`
  * Group Cohort Compartment: `GET /fhir/r4/{tenant}/Group/{id}/$export`
  * Single-Patient Compartment: `GET /fhir/r4/{tenant}/Patient/{id}/$export`
  * Async Job Status Poll & Cancel (Postgres-persisted): `GET /fhir/r4/{tenant}/$export-poll/{jobId}` & `DELETE /fhir/r4/{tenant}/$export-poll/{jobId}`
  * NDJSON File Download: `GET /fhir/r4/{tenant}/$export-file/{jobId}/{ResourceType}.ndjson`
* **Automated Conformance & Certification (`$certify`)**: `GET /fhir/r4/{tenant}/$certify` and `POST /fhir/r4/{tenant}/$certify` (executes 49 end-to-end conformance checks across FHIR R4, US Core v6.1.0 & v7.0.0, SMART v2, Terminology, Subscriptions, SDC, and ONC `(g)(10)` in an isolated sandbox and returns an HL7 FHIR R4 `TestReport` resource)
* **Validate**: `POST /fhir/r4/{tenant}/{resourceType}/$validate` and `POST /fhir/r4/{tenant}/$validate`
* **Search**: `GET /fhir/r4/{tenant}/{resourceType}?{searchParams}&_count=20&_offset=0` and `POST /fhir/r4/{tenant}/{resourceType}/_search` (includes `Bundle.link` `self`/`next`/`previous` pagination)
* **Security Audit Log (`AuditEvent`)**: `GET /fhir/r4/{tenant}/AuditEvent?entity=Patient/123&outcome=0&agent=Practitioner/dr-smith` and `GET /fhir/r4/{tenant}/AuditEvent/{id}`
* **CapabilityStatement**: `GET /fhir/r4/{tenant}/metadata`
* **Proto-Compiled OpenAPI 3.1.0 Spec**: `GET /fhir/r4/{tenant}/openapi.json` and `GET /fhir/r4/openapi.json`
* **Interactive API & Conformance Explorer (`$docs`)**: `GET /fhir/r4/{tenant}/$docs` and `GET /fhir/r4/$docs`
* **SMART Discovery**: `GET /fhir/r4/{tenant}/.well-known/smart-configuration`
* **Operational Probes & Prometheus Metrics**: `GET /healthz`, `GET /readyz`, and `GET /metrics` (also accessible under `/fhir/r4/{tenant}/healthz`, `/readyz`, `/metrics`)

### FHIR Conformance & Validation Engine (`pkg/validation`)
Flint validates incoming FHIR resources prior to persistence and exposes standard validation operations:
* **StructureDefinition Enforcement**: Validates elements against official HL7 FHIR R4 StructureDefinitions. Undeclared elements are rejected in strict mode (`structure`).
* **Cardinality Verification**: Mandatory 1..1 elements (e.g., `Observation.status`, `Observation.code`, `Condition.subject`, `Encounter.status`, `Encounter.class`) must be present and non-empty (`required`).
* **Required ValueSets**: Bound code elements (e.g., `AdministrativeGender`, `ObservationStatus`, `ConditionClinicalStatus`, `EncounterStatus`) are strictly matched against canonical ValueSets (`code-invalid`).
* **Primitive Format Constraints**: Primitive fields (`id`, `date`, `dateTime`, `instant`) are validated against FHIR regex patterns (`value`).
* **OperationOutcome Diagnostics**: Detailed, structured responses with issue severities, diagnostic reasons, and FHIRPath element expressions (e.g. `Observation.status`).
* **Standard `$validate` Operation**: Supports resource-level (`POST /fhir/r4/{tenant}/{resourceType}/$validate`) and system-level (`POST /fhir/r4/{tenant}/$validate`) operations, accepting raw JSON or FHIR `Parameters` payloads without persisting to storage.
* **Bundle Validation**: Transaction bundles validate all resource entries before commit, guaranteeing transaction atomicity.
* **Property-Based Testing**: Validated using generative property testing with [Hegel](https://hegel.dev) to ensure parser invariance and fuzzing safety.

### Advanced FHIR Search Engine
Flint implements high-performance relational search against the covering `spidx_*` index tables:
* **Date Range Queries (Precision-Aware Intervals)**: Supports FHIR prefix comparators (`eq`, `ne`, `lt`, `le`, `gt`, `ge`, `sa`, `eb`, `ap`) and precision-aware `YYYY` / `YYYY-MM` / `YYYY-MM-DD` intervals over `spidx_date`:
  ```http
  GET /fhir/r4/tenant-a/Patient?birthdate=ge1980-01-01&birthdate=le1995-12-31
  ```
* **Quantity Queries**: Supports comparator prefixes (`gt`, `lt`, `ge`, `le`, `eq`), systems, units, and multi-value ranges over `spidx_quantity`:
  ```http
  GET /fhir/r4/tenant-a/Observation?value-quantity=gt70|http://unitsofmeasure.org|kg
  ```
* **Search Modifiers (`:exact`, `:contains`, `:not`, `:missing`) & Multi-Field `_sort`**:
  ```http
  GET /fhir/r4/tenant-a/Patient?family:exact=Smith&given:contains=al&gender:not=male&_sort=-birthdate,family
  ```
* **Include, Reverse Include (`:iterate`) & Reverse Chaining (`_has`)**:
  ```http
  GET /fhir/r4/tenant-a/Observation?code=29463-7&_include=Observation:patient&_include:iterate=Patient:organization
  GET /fhir/r4/tenant-a/Patient?_has:Observation:patient:code=8867-4&_revinclude=Observation:patient
  ```
* **Chained Searches & Result Projection (`_summary`, `_elements`, `_total=none`)**:
  ```http
  GET /fhir/r4/tenant-a/Observation?patient.name=smith&_summary=true
  GET /fhir/r4/tenant-a/Patient?_elements=name,gender,birthDate&_total=none
  ```

---

## SMART on FHIR v2 & Identity Architecture

Flint functions as a high-performance **FHIR Resource Server (RS)** with built-in SMART on FHIR v2 OAuth2/PKCE (`S256`), refresh token rotation/downscoping, and RFC 7009 token revocation, while also supporting external OpenID Connect (OIDC) Identity Providers.

```mermaid
flowchart LR
    Client["SMART on FHIR App\n(EHR / Patient App)"]
    IdP["Identity Provider / Auth Server\n(Built-in SMART v2 / Zitadel / Keycloak / Okta)"]
    Flint["Flint FHIR Server\n(Resource Server)"]

    Client -->|1. Authenticate & Request Scopes| IdP
    IdP -->|2. Issue Signed Bearer JWT| Client
    Client -->|3. FHIR Request + Bearer Token| Flint
    Flint -->|4. Verify JWKS & Scopes via TokenValidator| Flint
    Flint -->|5. Authorized FHIR Bundle| Client
```

### Decoupled `TokenValidator` Interface (`pkg/auth`)
Flint protects endpoints through a pluggable interface:
* **`OIDCValidator`**: Standard OIDC and JWKS caching with RS256/ES256 verification and refresh rate-limiting (compatible with Okta, Azure AD, Keycloak, Auth0).
* **`ZitadelValidator`**: Specialized decorator mapping Zitadel organization domains to Flint tenant schemas, user metadata to patient IDs, and project roles to clinical SMART scopes.
* **`MockTokenValidator`**: In-memory ECDSA P-256 keypair signer and validator enabling sub-millisecond, zero-dependency unit tests.

### Security Guarantees
1. **SMART v1 & v2 Scopes (Including Granular Sub-Resource Filters)**: Enforces action permissions (`c`, `r`, `u`, `d`, `s`) and granular query constraints (e.g., `patient/Observation.rs?category=http://terminology.hl7.org/CodeSystem/observation-category|vital-signs`) across search, point reads, `vread`, `_history`, `PATCH`, `_include`, `_revinclude`, `_has`, `Patient/$everything`, and `$export`.
2. **Tenant Boundary Enforcement**: Rejects cross-tenant token access with HTTP 403.
3. **Patient Compartment Isolation**: When a token carries `patient/*` scopes or a `patient_id` launch claim:
   * Missing `patient_id` claims on `patient/*` tokens fail closed with `403 Forbidden`.
   * Reading, searching, or mutating records of another patient returns `403 Forbidden`.
   * Patient-scoped tokens are blocked from mutating shared non-compartment resources such as `Practitioner`.
4. **HIPAA / ONC Security Audit Logging (`AuditEvent`)**: Every clinical REST interaction (`read`, `vread`, `update`, `delete`, `history-instance`, `create`, `search-type`, `batch`, `everything`, `validate`) and security rejection (`401 Unauthorized` / `403 Forbidden`) is recorded in `fhir_audit_event` with actor identity, patient compartment attribution, client IP, and target entity reference, and is queryable via `GET /fhir/r4/{tenant}/AuditEvent`.

### Environment Configuration
```bash
# Enable SMART on FHIR with Zitadel
FLINT_OIDC_ISSUER="https://auth.azra.dev"
FLINT_OIDC_AUDIENCE="flint-api"
FLINT_AUTH_PROVIDER="zitadel"
FLINT_ZITADEL_TENANT_FROM_DOMAIN="true"
FLINT_SMART_AUTH_URL="https://auth.azra.dev/oauth/v2/authorize"
FLINT_SMART_TOKEN_URL="https://auth.azra.dev/oauth/v2/token"
```

---

## Property-Based Testing (via Hegel)

Flint leverages [Hegel](https://hegel.dev) (`hegel.dev/go/hegel`) for type-driven, generative property-based testing. Instead of checking only human-authored table cases, Hegel draws inputs across formal grammars and shrinks failures to minimal counterexamples:

* **Scope Grammar Invariance**: Validates that all strings matching the formal SMART v1/v2 grammar parse deterministically.
* **Query Filter Invariance**: Asserts that fine-grained query filters (`?category=vital-signs`) never alter base resource permissions.
* **Search Parameter Invariance**: Verifies parser prefix and grammar preservation across generated ISO date formats, decimal quantities, units, and `_include`/`_revinclude` strings.
* **Conditional Create (`If-None-Exist`) Invariance**: Verifies search criteria query string parsing and deterministic round-trip matching.
* **AuditEvent Classification & Serialization Invariance**: Verifies HTTP status-to-outcome partitioning (`0`, `4`, `8`) and round-trip FHIR R4 `AuditEvent` serialization across generated inputs.
* **No-Panic Invariance**: Fuzzes parsers with arbitrary inputs and unicode sequences to guarantee zero crashes or hangs.

---

## Optional Add-Ons (Streaming CDC, Temporal Workflows & Clinical Lakehouse)

By default, Flint builds, deploys, and runs **strictly as a standalone FHIR R4 server backed by PostgreSQL**. When needed, three opt-in add-on modules can be enabled via environment variables and `task` targets:

### 1. Streaming CDC Add-On (`pkg/cdc` + `automq/`)
The core FHIR server exposes a generic, zero-dependency [`cdc.Publisher`](pkg/cdc/cdc.go) interface (`srv.SetCDCPublisher(...)`). Setting `FLINT_AUTOMQ_BROKERS` on `flintd` activates the [`automq`](automq/) Kafka/AutoMQ producer adapter, streaming every FHIR resource upsert (`op=U`) and soft delete (`op=D`) to tenant-keyed Kafka/AutoMQ topics (`fhir.<ResourceType>`):

```bash
FLINT_POSTGRES_DSN="postgres://flint:flint@localhost:5432/flint?sslmode=disable" \
FLINT_AUTOMQ_BROKERS="localhost:9092" \
  bin/flintd
```

### 2. Temporal Ingestion Worker Add-On (`ingest/`, `cmd/worker/`)
For asynchronous, retryable batch bundle ingestion (`IngestFHIRBundle`), build and run the optional Temporal worker:

```bash
# Deploy optional backing services (Temporal, Garage S3, Iceberg REST, AutoMQ)
task addons-up

# Build and run the Temporal ingestion worker locally (or deploy via `task deploy-worker`)
task run-worker
```

### 3. Clinical Dimensional Lakehouse Add-On (`analytics/` — SQLMesh on DuckDB / Iceberg)
The optional [`analytics/`](analytics/) module provides an out-of-the-box **SQLMesh** analytical pipeline that transforms raw AutoMQ Table Topic Iceberg CDC streams (`fhir_raw.*`) into a Kimball healthcare dimensional model (`flint_stg.stg_*`, `flint_marts.dim_*`, `flint_marts.fct_*`, `mart_patient_360`, `mart_ecqm_cms165_bp_control`, `mart_ecqm_cms122_diabetes_a1c`, and `mart_claims_utilization`):

```bash
# Run the hermetic SQLMesh unit test suite (22/22 DuckDB tests)
task test-analytics

# Verify AutoMQ Table Topic restart recovery
task test-recovery
```

---

## Project Structure

```
flint/
├── bin/                  # Compiled binaries (flintd, seed, certify, loadtest; optional: worker)
├── cmd/
│   ├── flintd/           # Core FHIR REST HTTP server entrypoint
│   ├── seed/             # Deterministic US Core v6.1.0 & v7.0.0 cohort seeder (task seed)
│   ├── certify/          # Automated 50-check US Core v6.1.0 & v7.0.0 / ONC (g)(10) certification CLI (task certify)
│   ├── loadtest/         # High-throughput HTTP & Postgres store load testing harness
│   ├── worker/           # [Add-On] Temporal ingestion worker entrypoint (task build-worker)
│   └── e2e/              # [Add-On] Lakehouse E2E & restart recovery verification
├── deploy/
│   ├── inferno/          # Official HL7 Inferno US Core Test Kit Docker Compose (task certify-inferno)
│   ├── k3d/              # Local k3d cluster configuration
│   └── k8s/              # Kubernetes manifests (Core: postgres.yaml, flintd.yaml; Add-Ons: worker, temporal, automq, garage, iceberg-rest)
├── fhir/r4/              # Machine-readable HL7 FHIR R4 SearchParameter definitions
├── gen/go/
│   ├── flint/auth/v1/    # Protobuf-generated security context and discovery types
│   └── store/postgres/   # proto2type generated search index extractors
├── pkg/
│   ├── audit/            # HIPAA/ONC FHIR R4 AuditEvent classification, serialization & recorders
│   ├── auth/             # TokenValidator, OIDC/Zitadel, and Hegel property tests
│   ├── cdc/              # Generic, zero-dependency CDC Publisher interface & in-memory sink
│   ├── certify/          # 50-check FHIR R4 / US Core v6.1.0 & v7.0.0 / ONC (g)(10) conformance engine & TestReport builder
│   ├── fhirutil/         # Bidirectional flat FHIR R4 JSON <-> google/fhir protojson codec & helpers
│   └── validation/       # FHIR StructureDefinition, ValueSet, and cardinality validator
├── proto/
│   ├── flint/auth/v1/    # Canonical Protobuf schemas for auth & security context
│   └── google/fhir/      # Vendored google/fhir R4 protocol buffers
├── server/               # stdlib HTTP router, Bundle engine, Terminology, Subscriptions, SMART v2 OAuth2, auth/audit middleware
├── store/postgres/       # Operational store, Atlas SQL migrations, spidx_* tables, OAuth2 & $export job persistence
├── automq/               # [Add-On] Franz-go Kafka/AutoMQ CDC adapter implementing pkg/cdc.Publisher
├── ingest/               # [Add-On] Temporal IngestFHIRBundle workflow & activities
├── analytics/            # [Add-On] SQLMesh clinical dimensional lakehouse models & DuckDB unit tests
├── flake.nix             # Hermetic Nix development environment
├── lefthook.yml          # Git pre-commit & pre-push hooks
└── Taskfile.yaml         # Project workflow automation tasks
```

---

## Community & Contributing

We welcome contributions! Please see our:
* [Code of Conduct](CODE_OF_CONDUCT.md)
* [Contributing Guide](CONTRIBUTING.md)
* [Security Policy](SECURITY.md)

---

## License

Flint is licensed under the [Apache License 2.0](LICENSE).
