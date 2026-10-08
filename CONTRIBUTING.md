# Contributing to Flint

Thank you for your interest in contributing to **Flint**! Flint is an open-source, high-throughput FHIR R4 server and data lakehouse engine built on Postgres, AutoMQ, and Apache Iceberg.

Please take a moment to review this guide before submitting issues or pull requests.

---

## Code of Conduct

All contributors and participants are expected to adhere to our [Code of Conduct](CODE_OF_CONDUCT.md). Please report any violations to [maintainers@flint-fhir.org](mailto:maintainers@flint-fhir.org).

---

## Development Environment Setup

Flint provides a hermetic development environment using **Nix flakes** and **direnv**, as well as containerized local infrastructure with **k3d**.

### Prerequisites

* [Nix](https://nixos.org/download.html) with Flakes enabled (or Go 1.25+, Buf, and Docker)
* [direnv](https://direnv.net/) (recommended)
* [Docker](https://www.docker.com/) (for k3d local cluster)

### Quick Start with Nix & Direnv

1. Clone the repository:
   ```bash
   git clone https://github.com/flint-fhir/flint.git
   cd flint
   ```

2. Allow direnv to load the hermetic Nix dev shell:
   ```bash
   direnv allow
   ```
   *(Or manually enter the shell via `nix develop`)*

3. Verify toolchain and hooks:
   ```bash
   lefthook install
   go version
   buf --version
   ```

---

## Local Development Stack

Flint uses `k3d` to run local dependencies (Postgres, Temporal, AutoMQ StatefulSet, Garage S3, and Iceberg REST catalog):

```bash
# Spin up local cluster with all services
task dev

# Inspect status of running pods
task status

# Build binaries (bin/flintd, bin/worker, bin/loadtest)
task build

# Run flintd FHIR server locally
task run

# Run Temporal worker locally
task run-worker

# Run integration tests against local Postgres
task test

# Tear down local cluster
task down
```

---

## Code Generation (`proto2type` & `buf`)

All FHIR domain models and search index extractors are generated from Protocol Buffers using [proto2type](https://github.com/protocgen/proto2type) and [Buf](https://buf.build).

To regenerate code after modifying protobuf definitions or search parameters:

```bash
buf generate
```

Verify linting across protobuf schemas:
```bash
buf lint
```

---

## Testing & Quality Assurance

Before opening a pull request, ensure all tests and linters pass:

```bash
# Run unit tests
go test ./...

# Run integration tests against real Postgres
FLINT_TEST_POSTGRES_DSN="postgres://flint:flint@localhost:5432/flint?sslmode=disable" go test ./...

# Run linter
golangci-lint run

# Run AutoMQ restart recovery verification
task test-recovery

# Run high-throughput load tests
task bench-http
task bench-store
```

---

## Git Workflow & Conventional Commits

1. **Create a dedicated branch**:
   Always work from a descriptive feature branch:
   ```bash
   git checkout -b feat/my-new-feature
   ```

2. **Conventional Commits**:
   Commit messages must follow the [Conventional Commits](https://www.conventionalcommits.org/) specification:
   * `feat:` new features or resource extractors
   * `fix:` bug fixes
   * `perf:` performance improvements
   * `docs:` documentation updates
   * `chore:` build, tooling, or dependency changes
   * `ci:` CI/CD pipeline modifications

3. **Pre-commit Hooks**:
   Pre-commit hooks are managed by `lefthook` and automatically run `gofmt`, `go vet`, `buf lint`, and `golangci-lint`. Do **not** bypass hooks with `--no-verify`.

4. **Pull Requests**:
   * Open PRs against `main`.
   * Ensure GitHub Actions CI passes (`CI/buf`, `CI/lint`, `CI/test`).
   * Include a clear description of the problem, solution, and local verification steps.
