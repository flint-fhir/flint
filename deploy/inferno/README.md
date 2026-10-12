# HL7 Inferno US Core v6.1.0 & ONC `(g)(10)` Certification Harness

This directory provides a pre-configured Docker Compose stack for running the official **[HL7 Inferno US Core Test Kit](https://github.com/inferno-framework/us-core-test-kit)** against a local Flint server.

## 1. Quick Automated Certification (No Docker Required)

Run Flint's built-in 24-check FHIR R4 / US Core v6.1.0 / ONC `(g)(10)` Conformance & Certification Runner (`cmd/certify`):

```bash
# Prints the full Certification Scorecard and writes a FHIR R4 TestReport JSON
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
2. Launch the HL7 Inferno US Core test kit:
   ```bash
   task certify-inferno
   ```
3. Open **[http://localhost:4567](http://localhost:4567)** in your browser:
   - Select **US Core v6.1.0** (USCDI v3)
   - Set **FHIR Endpoint** to: `http://host.docker.internal:8080/fhir/r4/demo`
   - Set **Patient IDs** to: `pat-alice-001,pat-robert-002,pat-elena-003`
   - Set **Group ID** (for Bulk Data `$export`) to: `all`
