#!/usr/bin/env bash
set -euo pipefail

# Headless HL7 AU Core (v1.0.0 & v2.0.0) Conformance Runner
# Isolated from deploy/inferno/headless due to au_core_test_kit's tls_test_kit ~> 0.2.0 dependency.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
AU_DIR="${SCRIPT_DIR}/au_core"
HEADLESS_DIR="${SCRIPT_DIR}/headless"

FLINT_BASE_URL="${FLINT_BASE_URL:-http://127.0.0.1:8080}"
FLINT_BASE_URL="${FLINT_BASE_URL%/}"
FLINT_AU_TENANT="${FLINT_AU_TENANT:-aucore}"
FLINT_AU_FHIR_URL="${FLINT_AU_FHIR_URL:-${FLINT_BASE_URL}/fhir/r4/${FLINT_AU_TENANT}}"
INFERNO_ARTIFACTS_DIR="${INFERNO_ARTIFACTS_DIR:-${REPO_ROOT}/artifacts/inferno-au-core}"
VALIDATOR_BRIDGE_PORT="${VALIDATOR_BRIDGE_PORT:-14568}"

mkdir -p "${INFERNO_ARTIFACTS_DIR}" "${AU_DIR}/data"

BRIDGE_PID=""
cleanup() {
  if [[ -n "${BRIDGE_PID}" ]]; then
    kill "${BRIDGE_PID}" 2>/dev/null || true
    wait "${BRIDGE_PID}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

echo "==> Verifying Flint readiness & SMART discovery at ${FLINT_AU_FHIR_URL}"
curl -fsS "${FLINT_BASE_URL}/readyz" >/dev/null
SMART_CFG="$(curl -fsS "${FLINT_AU_FHIR_URL}/.well-known/smart-configuration")"
TOKEN_URL="$(ruby -rjson -e 'puts JSON.parse(STDIN.read).fetch("token_endpoint")' <<< "${SMART_CFG}")"

echo "==> Minting SMART v2 system/*.cruds access token from ${TOKEN_URL}"
TOKEN_RESP="$(curl -fsS -X POST "${TOKEN_URL}" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode 'grant_type=client_credentials' \
  --data-urlencode 'client_id=inferno-au-core-seed' \
  --data-urlencode 'scope=system/*.cruds')"
ACCESS_TOKEN="$(ruby -rjson -e 'puts JSON.parse(STDIN.read).fetch("access_token")' <<< "${TOKEN_RESP}")"

echo "==> Seeding HL7 AU Core v1.0.0 & v2.0.0 clinical cohort into tenant '${FLINT_AU_TENANT}'"
if [[ -x "${REPO_ROOT}/bin/seed" ]]; then
  "${REPO_ROOT}/bin/seed" -url "${FLINT_BASE_URL}" -tenant "${FLINT_AU_TENANT}" -suite aucore -token "${ACCESS_TOKEN}" >/dev/null
else
  (cd "${REPO_ROOT}" && go run ./cmd/seed -url "${FLINT_BASE_URL}" -tenant "${FLINT_AU_TENANT}" -suite aucore -token "${ACCESS_TOKEN}" >/dev/null)
fi

cd "${AU_DIR}"

if ! bundle check >/dev/null 2>&1; then
  echo "==> Installing HL7 AU Core Inferno Ruby gems"
  bundle install --jobs 4 --retry 3
fi

if [[ -z "${FHIR_RESOURCE_VALIDATOR_URL:-}" ]]; then
  echo "==> Starting Flint-backed HL7 Validator Wrapper bridge on 127.0.0.1:${VALIDATOR_BRIDGE_PORT}"
  FLINT_FHIR_URL="${FLINT_AU_FHIR_URL}" \
  FLINT_VALIDATOR_TOKEN="${ACCESS_TOKEN}" \
  VALIDATOR_BRIDGE_PORT="${VALIDATOR_BRIDGE_PORT}" \
  bundle exec ruby "${HEADLESS_DIR}/validator_bridge.rb" > "${INFERNO_ARTIFACTS_DIR}/validator_bridge.log" 2>&1 &
  BRIDGE_PID=$!

  for _ in $(seq 1 30); do
    if curl -fsS "http://127.0.0.1:${VALIDATOR_BRIDGE_PORT}/health" >/dev/null 2>&1; then
      break
    fi
    sleep 0.25
  done
  curl -fsS "http://127.0.0.1:${VALIDATOR_BRIDGE_PORT}/health" >/dev/null
  export FHIR_RESOURCE_VALIDATOR_URL="http://127.0.0.1:${VALIDATOR_BRIDGE_PORT}"
fi
export FHIRPATH_URL="${FHIRPATH_URL:-${FHIR_RESOURCE_VALIDATOR_URL}}"

echo "==> Running AU Core Inferno SQLite migrations"
rm -f "${AU_DIR}/data/inferno_au_ci.db" "${AU_DIR}/data/inferno_au_ci.db-shm" "${AU_DIR}/data/inferno_au_ci.db-wal"
INFERNO_DB="${AU_DIR}/data/inferno_au_ci.db" ASYNC_JOBS="false" bundle exec inferno migrate >/dev/null

echo "==> Executing HL7 AU Core v1.0.0 & v2.0.0 Inferno suites against ${FLINT_AU_FHIR_URL}"
INFERNO_DB="${AU_DIR}/data/inferno_au_ci.db" \
ASYNC_JOBS="false" \
INFERNO_DISABLE_TLS_TEST="true" \
FLINT_BASE_URL="${FLINT_BASE_URL}" \
FLINT_AU_TENANT="${FLINT_AU_TENANT}" \
FLINT_AU_FHIR_URL="${FLINT_AU_FHIR_URL}" \
INFERNO_ACCESS_TOKEN="${ACCESS_TOKEN}" \
INFERNO_ARTIFACTS_DIR="${INFERNO_ARTIFACTS_DIR}" \
bundle exec ruby "${AU_DIR}/run_au_core.rb"
