# Security Policy

Flint is designed for healthcare data infrastructure where patient privacy, HIPAA compliance, and data integrity are paramount. We take security vulnerabilities seriously and appreciate the efforts of the security research community to help keep Flint secure.

---

## Supported Versions

Security updates are actively maintained on the latest minor version branch:

| Version | Supported          |
|:--------|:-------------------|
| `main`  | :white_check_mark: |

---

## Reporting a Vulnerability

If you discover a security vulnerability in Flint, **please do not disclose it publicly or file an open GitHub issue.**

Please report security issues via email to:

📧 **[security@flint-fhir.org](mailto:security@flint-fhir.org)**

### Information to Include

To help us triage and resolve the issue quickly, please include:
* A detailed description of the vulnerability and its potential impact.
* Step-by-step instructions or proof-of-concept code to reproduce the issue.
* Affected components (`flintd`, Postgres store, AutoMQ producer, Temporal worker, or generated extractors).
* Any mitigations you have identified.

### Response Timeline

* **Acknowledgment**: We aim to acknowledge receipt of security reports within **48 hours**.
* **Assessment & Fix**: We will provide an assessment and work toward an advisory and patch release as quickly as feasible.
* **Public Disclosure**: Once a fix is released, we will coordinate public disclosure with appropriate credit to the reporter.

---

## Healthcare & Security Architecture Principles

Flint incorporates several security architectural guarantees by design:

* **Physical Multi-Tenant Isolation**: Schema-per-tenant isolation at the Postgres level ensures queries and transactions can never cross tenant boundaries.
* **Lossless Protobuf Storage**: FHIR resources are stored in native Protocol Buffer format rather than parsed JSON strings, mitigating injection attacks and schema drift.
* **Audit Trail**: Temporal workflow history and AutoMQ CDC change streams provide cryptographically timestamped, immutable audit trails for every resource creation, update, and deletion.
* **HIPAA Compliance**: Production deployments should ensure TLS 1.3 in-transit encryption and KMS customer-managed encryption keys for object storage (S3/Garage) and operational databases.
