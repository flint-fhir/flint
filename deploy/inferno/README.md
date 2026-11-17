# HL7 Inferno US Core v6.1.0 & ONC `(g)(10)` Certification Harness

This directory provides a pre-configured Docker Compose stack for running the official **[HL7 Inferno ONC `(g)(10)` / US Core Test Kit](https://github.com/inferno-framework/onc-certification-g10-test-kit)** against a local Flint server.

## 1. Quick Automated Certification (No Docker Required)

Run Flint's built-in 138-check FHIR R4 / US Core v6.1.0 & v7.0.0 / SMART v2 / ONC `(g)(10)` & HTI-1 `(b)(11)` / SQL-on-FHIR v2 / National IG Conformance & Certification Runner (`cmd/certify`):

```bash
# Prints the full 138/138 Certification Scorecard across 10 suites and writes a FHIR R4 TestReport JSON
task certify
```

Or invoke the live server operation directly:

```bash
curl -s -X POST http://localhost:8080/fhir/r4/demo/$certify | jq .
```

## 2. Running Official HL7 Inferno UI (`localhost:4567`)

1. Start Flint and seed the 25-resource 11-profile US Core v6.1.0 cohort:
   ```bash
   task run
   task seed
   ```
2. Launch the HL7 Inferno ONC `(g)(10)` / US Core test kit:
   ```bash
   task certify-inferno
   ```
3. Open **[http://localhost:4567](http://localhost:4567)** in your browser:
   - Select **US Core v6.1.0** (USCDI v3) or **ONC Certification `(g)(10)` Standardized API**
   - **FHIR Endpoint**: `http://host.docker.internal:8080/fhir/r4/demo`
   - **Patient IDs**: `pat-alice-001,pat-robert-002,pat-elena-003`
   - **Group ID** (for Bulk Data `$export`): `all`
   - **OAuth2 / SMART App Launch v2.0.0 Endpoints** (auto-discovered via `/.well-known/smart-configuration` and `/.well-known/openid-configuration`):
     - **Authorize URL**: `http://host.docker.internal:8080/oauth2/authorize`
     - **Token URL**: `http://host.docker.internal:8080/oauth2/token`
     - **Introspection URL**: `http://host.docker.internal:8080/oauth2/introspect`
     - **JWKS URL**: `http://host.docker.internal:8080/fhir/r4/demo/.well-known/jwks.json`
     - **Client ID**: `inferno`


## 3. Headless HL7 Inferno CLI & GitHub Actions CI (`deploy/inferno/headless`)

In addition to the interactive Docker UI above, Flint runs the official HL7 Inferno Ruby test kits headlessly in-process (SQLite-backed `Inferno::Application.start(:executor)` — no Docker, Redis, Sidekiq, or multi-GB Java validator container required):

- **`smart_app_launch_test_kit` (`smart_stu2` & `smart_stu2_asymmetric`)**: Full SMART App Launch v2.0.0 (`smart_stu2`) conformance — Discovery (`/.well-known/smart-configuration` & `/.well-known/openid-configuration`), Standalone Launch (`GET /oauth2/authorize` with PKCE `S256`, RS256 OIDC `id_token` & `fhirUser` retrieval), EHR Launch (`POST /oauth2/launch` + `POST /oauth2/authorize` `authorize-post` with confidential symmetric `client_secret_basic`), Confidential Asymmetric (`ES384` `private_key_jwt`) Standalone Launch & Refresh Token Rotation (with and without `scope`), Backend Services client authentication, and RFC 7662 Token Introspection (`POST /oauth2/introspect`) against live `flintd` with `FLINT_SMART_ENFORCE_DEV_AUTH=true`.
- **`bulk_data_test_kit` (`bulk_data_v200`)**: FHIR Bulk Data Access IG v2.0.0 (`Group/all/$export`, `202 Accepted` `Content-Location` polling, `200 OK` manifest validation, NDJSON file downloads, and `DELETE` export cancellation).
- **`us_core_test_kit` (`us_core_v610`, `us_core_v700`, & `us_core_v800`)**: US Core IG v6.1.0 (USCDI v3), v7.0.0 (USCDI v4), and v8.0.0 (USCDI v5) FHIR REST API conformance across the seeded multi-patient cohort, backed by `deploy/inferno/headless/validator_bridge.rb` delegating `/validate` requests to Flint's native `/{resourceType}/$validate?profile=...` engine.
- **`onc_certification_g10_test_kit` (`g10_certification`)**: ONC § 170.315(g)(10) / HTI-1 Standardized API for Patient and Population Services certification suite.
- **`carin_for_blue_button_test_kit` (`c4bb_v200`)**, **`davinci_pdex_test_kit` (`pdex_payer_server`)**, **`davinci_pas_test_kit` (`davinci_pas_server_suite_v221`)**, **`davinci_plan_net_test_kit` (`davinci_plan_net_server_v110`)**, and **`davinci_us_drug_formulary_test_kit` (`davinci_us_drug_formulary_v201`)**: CMS-0057-F Interoperability & Prior Authorization Rule suites (Patient Access Claims/EOB, Payer-to-Payer `$member-match`/`$everything`/`$export`, Prior Authorization `$submit`/`$inquire`/Claim Updates/Pended Subscriptions, Provider Directory, and US Drug Formulary).
- **`ips_test_kit` (`ips_v110`)**, **`ipa_test_kit` (`ipa_v100`)**, and **`subscriptions_test_kit` (`subscriptions_r5_backport_r4_server`)**: HL7 International Patient Summary (`$summary`), International Patient Access, and R5 Subscriptions Backport (`rest-hook` handshake/heartbeat/event-notification `Bundle`s & `$status`).

Run locally against a running `flintd` instance (requires Ruby 3.3+ and Bundler):

```bash
task certify-inferno-headless
# or directly:
FLINT_BASE_URL=http://127.0.0.1:8080 FLINT_TENANT=demo ./deploy/inferno/run-headless.sh
```

Per-suite JSON reports (`smart_stu2.json`, `bulk_data_v200.json`, `us_core_v610.json`, and `summary.json`) are written to `artifacts/inferno/` and uploaded automatically on every push and pull request by [`.github/workflows/inferno.yml`](../../.github/workflows/inferno.yml) across both `--store=postgres` and `--store=sqlite`.
