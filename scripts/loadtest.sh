#!/usr/bin/env bash
# loadtest.sh — HTTP load test for Credo examples using hey.
#
# Usage:
#   bash scripts/loadtest.sh
#   REQUESTS=50000 CONCURRENCY=100 bash scripts/loadtest.sh
#
# Prerequisites:
#   go install github.com/rakyll/hey@latest
set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration (override via environment)
# ---------------------------------------------------------------------------
REQUESTS="${REQUESTS:-10000}"
CONCURRENCY="${CONCURRENCY:-50}"
PORT="${PORT:-8099}"
HOST="${HOST:-127.0.0.1}"
BASE_URL="http://${HOST}:${PORT}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
TMPBIN="${TMPDIR:-/tmp}"

# ---------------------------------------------------------------------------
# Prerequisites
# ---------------------------------------------------------------------------
check_prereqs() {
    if ! command -v hey &>/dev/null; then
        echo "ERROR: 'hey' not found on PATH."
        echo "Install: go install github.com/rakyll/hey@latest"
        exit 1
    fi
    if ! command -v go &>/dev/null; then
        echo "ERROR: 'go' not found on PATH."
        exit 1
    fi
}

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
SERVER_PID=""

cleanup() {
    if [[ -n "${SERVER_PID}" ]]; then
        kill "${SERVER_PID}" 2>/dev/null || true
        wait "${SERVER_PID}" 2>/dev/null || true
        SERVER_PID=""
    fi
}
trap cleanup EXIT

wait_for_server() {
    local url="$1"
    local attempts=0
    local max_attempts=50
    while ! curl -sf "${url}" >/dev/null 2>&1; do
        attempts=$((attempts + 1))
        if [[ ${attempts} -ge ${max_attempts} ]]; then
            echo "ERROR: Server did not start within ${max_attempts} attempts."
            exit 1
        fi
        sleep 0.1
    done
}

header() {
    echo ""
    echo "================================================================"
    echo "  $1"
    echo "================================================================"
    echo ""
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
check_prereqs

echo "Load test config: ${REQUESTS} requests, ${CONCURRENCY} concurrency, port ${PORT}"

# --- Hello example ---
header "Building examples/hello"
cd "${PROJECT_ROOT}/examples/hello"
go build -o "${TMPBIN}/credo-bench-hello" .

header "Starting hello server on :${PORT}"
CREDO_SERVER__HOST="${HOST}" CREDO_SERVER__PORT="${PORT}" "${TMPBIN}/credo-bench-hello" &
SERVER_PID=$!
wait_for_server "${BASE_URL}/"

header "Load test: GET / (static route)"
hey -n "${REQUESTS}" -c "${CONCURRENCY}" "${BASE_URL}/"

header "Load test: GET /hello/world (param route)"
hey -n "${REQUESTS}" -c "${CONCURRENCY}" "${BASE_URL}/hello/world"

cleanup

# --- SaaS example ---
header "Building examples/saas"
cd "${PROJECT_ROOT}/examples/saas"
go build -o "${TMPBIN}/credo-bench-saas" .

header "Starting saas server on :${PORT}"
CREDO_SERVER__HOST="${HOST}" CREDO_SERVER__PORT="${PORT}" "${TMPBIN}/credo-bench-saas" &
SERVER_PID=$!
wait_for_server "${BASE_URL}/health"

header "Load test: GET /health (public, with global middleware)"
hey -n "${REQUESTS}" -c "${CONCURRENCY}" "${BASE_URL}/health"

header "Load test: POST /auth/login (public, JWT generation)"
hey -n "${REQUESTS}" -c "${CONCURRENCY}" -m POST "${BASE_URL}/auth/login"

# Get a JWT token for authenticated endpoint testing
TOKEN=$(curl -sf -X POST "${BASE_URL}/auth/login" | grep -o '"token":"[^"]*"' | cut -d'"' -f4)
if [[ -n "${TOKEN}" ]]; then
    header "Load test: GET /api/v1/me (authenticated, JWT validation)"
    hey -n "${REQUESTS}" -c "${CONCURRENCY}" \
        -H "Authorization: Bearer ${TOKEN}" \
        "${BASE_URL}/api/v1/me"
fi

cleanup

# --- Summary ---
header "Done"
echo "All load tests completed."
echo "  Tool:        hey"
echo "  Requests:    ${REQUESTS}"
echo "  Concurrency: ${CONCURRENCY}"
echo ""
echo "For Go benchmarks, run:  make bench"
