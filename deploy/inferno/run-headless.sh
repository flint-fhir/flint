#!/usr/bin/env bash
set -euo pipefail

# Headless HL7 Inferno Conformance Runner (US Core v6.1.0 + SMART STU2 + Bulk Data v2.0.0)
#
# Runs official HL7 Inferno Ruby test suites directly against a live Flint server
# without requiring Docker, Redis, Sidekiq, or a 2 GB Java validator container.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
HEADLESS_DIR="${SCRIPT_DIR}/headless"

FLINT_BASE_URL="${FLINT_BASE_URL:-http://127.0.0.1:8080}"
FLINT_BASE_URL="${FLINT_BASE_URL%/}"
FLINT_TENANT="${FLINT_TENANT:-demo}"
FLINT_FHIR_URL="${FLINT_FHIR_URL:-${FLINT_BASE_URL}/fhir/r4/${FLINT_TENANT}}"
INFERNO_ARTIFACTS_DIR="${INFERNO_ARTIFACTS_DIR:-${REPO_ROOT}/artifacts/inferno}"
VALIDATOR_BRIDGE_PORT="${VALIDATOR_BRIDGE_PORT:-14568}"

mkdir -p "${INFERNO_ARTIFACTS_DIR}" "${HEADLESS_DIR}/data"

BRIDGE_PID=""
cleanup() {
  if [[ -n "${BRIDGE_PID}" ]]; then
    kill "${BRIDGE_PID}" 2>/dev/null || true
    wait "${BRIDGE_PID}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

echo "==> Verifying Flint readiness & SMART v2 discovery at ${FLINT_FHIR_URL}"
curl -fsS "${FLINT_BASE_URL}/readyz" >/dev/null
SMART_CFG="$(curl -fsS "${FLINT_FHIR_URL}/.well-known/smart-configuration")"
TOKEN_URL="$(ruby -rjson -e 'puts JSON.parse(STDIN.read).fetch("token_endpoint")' <<< "${SMART_CFG}")"

echo "==> Minting SMART v2 system/*.cruds access token from ${TOKEN_URL}"
TOKEN_RESP="$(curl -fsS -X POST "${TOKEN_URL}" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode 'grant_type=client_credentials' \
  --data-urlencode 'client_id=inferno-headless-seed' \
  --data-urlencode 'scope=system/*.cruds')"
ACCESS_TOKEN="$(ruby -rjson -e 'puts JSON.parse(STDIN.read).fetch("access_token")' <<< "${TOKEN_RESP}")"

if [[ -z "${INFERNO_PATIENT_IDS:-}" ]]; then
  PAT_Check="$(curl -fsS -H "Authorization: Bearer ${ACCESS_TOKEN}" "${FLINT_FHIR_URL}/Patient/pat-alice-001" || true)"
  if [[ -z "${PAT_Check}" ]] || ! grep -q '"pat-alice-001"' <<< "${PAT_Check}"; then
    echo "==> Seeding 29-resource US Core v6.1.0 & v7.0.0 clinical cohort into tenant '${FLINT_TENANT}'"
    if [[ -x "${REPO_ROOT}/bin/seed" ]]; then
      "${REPO_ROOT}/bin/seed" -url "${FLINT_BASE_URL}" -tenant "${FLINT_TENANT}" -token "${ACCESS_TOKEN}"
    else
      (cd "${REPO_ROOT}" && go run ./cmd/seed -url "${FLINT_BASE_URL}" -tenant "${FLINT_TENANT}" -token "${ACCESS_TOKEN}")
    fi
  fi
  INFERNO_PATIENT_IDS="pat-alice-001,pat-robert-002,pat-elena-003"
fi

cd "${HEADLESS_DIR}"

if ! bundle check >/dev/null 2>&1; then
  echo "==> Installing HL7 Inferno Ruby gems (inferno_core, us_core_test_kit, smart_app_launch_test_kit, bulk_data_test_kit)"
  bundle install --jobs 4 --retry 3
fi

if [[ -z "${FHIR_RESOURCE_VALIDATOR_URL:-}" ]]; then
  echo "==> Starting Flint-backed HL7 Validator Wrapper bridge on 127.0.0.1:${VALIDATOR_BRIDGE_PORT}"
  FLINT_FHIR_URL="${FLINT_FHIR_URL}" \
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

echo "==> Running Inferno SQLite schema migrations"
INFERNO_DB="${HEADLESS_DIR}/data/inferno_ci.db" bundle exec inferno migrate >/dev/null

echo "==> Executing official HL7 Inferno suites against ${FLINT_FHIR_URL}"
INFERNO_DB="${HEADLESS_DIR}/data/inferno_ci.db" \
INFERNO_DISABLE_TLS_TEST="true" \
FLINT_BASE_URL="${FLINT_BASE_URL}" \
FLINT_TENANT="${FLINT_TENANT}" \
FLINT_FHIR_URL="${FLINT_FHIR_URL}" \
INFERNO_PATIENT_IDS="${INFERNO_PATIENT_IDS}" \
INFERNO_ACCESS_TOKEN="${ACCESS_TOKEN}" \
INFERNO_ARTIFACTS_DIR="${INFERNO_ARTIFACTS_DIR}" \
bundle exec ruby "${HEADLESS_DIR}/run_suites.rb"
