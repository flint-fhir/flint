# HL7 Inferno US Core v6.1.0 & ONC `(g)(10)` Certification Harness

This directory provides a pre-configured Docker Compose stack for running the official **[HL7 Inferno ONC `(g)(10)` / US Core Test Kit](https://github.com/inferno-framework/onc-certification-g10-test-kit)** against a local Flint server.

## 1. Quick Automated Certification (No Docker Required)

Run Flint's built-in 82-check FHIR R4 / US Core v6.1.0 & v7.0.0 / SMART v2 / ONC `(g)(10)` & HTI-1 `(b)(11)` Conformance & Certification Runner (`cmd/certify`):

```bash
# Prints the full 82/82 Certification Scorecard across 9 suites and writes a FHIR R4 TestReport JSON
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

