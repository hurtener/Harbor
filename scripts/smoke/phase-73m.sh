#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 73m — Console Settings page + harbor console subcommand (D-129).
#
# Surface assertions (404/405/501 auto-SKIP per AGENTS.md §4.2):
#   1. auth.rotate_token route (POST /v1/auth/rotate_token) exists and is
#      auth-gated — a request with no bearer token is rejected (401/403).
#   2. harbor console subcommand boots: `harbor console --help` exits 0,
#      and `harbor console --bind 127.0.0.1:0` serves the embedded build
#      at `/` (200 OK).
#   3. harbor dev --help does NOT advertise a console-serving flag
#      (D-091 binding rule).
#
# NOTE: runtime.info / runtime.health / runtime.counters / runtime.drivers /
# metrics.snapshot are owned by Phase 72f; governance.posture + llm.posture
# are owned by Phase 72g. Their smokes live in their respective phase
# files; 73m only asserts auth.rotate_token + the harbor console boot.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. auth.rotate_token route exists + is auth-gated. The preflight
#    server runs the JWT auth.Middleware: a POST with no bearer token is
#    rejected 401 (CodeIdentityRequired). A 404/405/501 means the route
#    is not mounted yet -> SKIP (the AGENTS.md §4.2 convention).
ROTATE_URL="$(api_url /v1/auth/rotate_token)"
if command -v curl >/dev/null 2>&1; then
  rotate_status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${ROTATE_URL}" \
    || echo "000")
  case "${rotate_status}" in
    404|405|501|000)
      skip "phase 73m: auth.rotate_token route absent (${rotate_status})"
      ;;
    401|403)
      ok "phase 73m: auth.rotate_token route is auth-gated (HTTP ${rotate_status} without a valid admin token)"
      ;;
    *)
      fail "phase 73m: auth.rotate_token unauthenticated POST expected 401/403, got ${rotate_status}"
      ;;
  esac
else
  skip 'phase 73m: curl not available — auth.rotate_token probe deferred'
fi

# 2. harbor console subcommand smoke — CLI side.
if [ -x "./bin/harbor" ]; then
  if ./bin/harbor console --help >/dev/null 2>&1; then
    ok 'phase 73m: harbor console --help exits 0'

    # Boot harbor console on an ephemeral port and assert it serves the
    # embedded Console index at `/`. The embedded zero-config default
    # boots in-memory drivers + the mock LLM.
    CONSOLE_DATADIR="$(mktemp -d "${TMPDIR:-/tmp}/harbor-console-smoke.XXXXXX")"
    CONSOLE_LOG="${CONSOLE_DATADIR}/console.stderr"
    (
      cd "${CONSOLE_DATADIR}"
      HARBOR_DEV_ALLOW_MOCK=1 "${ROOT}/bin/harbor" console --bind 127.0.0.1:0 \
        >/dev/null 2>"${CONSOLE_LOG}" &
      echo $! > "${CONSOLE_DATADIR}/console.pid"
    )
    CONSOLE_PID="$(cat "${CONSOLE_DATADIR}/console.pid")"

    # Wait (bounded) for the HARBOR_*_BOUND= line. The `grep` runs
    # standalone (no pipeline) so `set -o pipefail` cannot abort the
    # script on a SIGPIPE; a no-match is tolerated via `|| true`.
    console_url=""
    attempt=0
    while [ "${attempt}" -lt 100 ]; do
      bound_line="$(grep -oE 'HARBOR_(DEV_)?BOUND=[^[:space:]]+' "${CONSOLE_LOG}" 2>/dev/null || true)"
      if [ -n "${bound_line}" ]; then
        console_url="http://${bound_line##*=}"
        break
      fi
      attempt=$((attempt + 1))
      sleep 0.1
    done

    if [ -n "${console_url}" ]; then
      console_status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        "${console_url}/" || echo "000")
      if [ "${console_status}" = "200" ]; then
        ok "phase 73m: harbor console serves the embedded Console build at / (200 OK)"
      else
        fail "phase 73m: harbor console GET / expected 200, got ${console_status}"
      fi
    else
      fail 'phase 73m: harbor console did not emit a bound-port line'
    fi

    # Tear down the harbor console child + temp dir.
    kill "${CONSOLE_PID}" 2>/dev/null || true
    wait "${CONSOLE_PID}" 2>/dev/null || true
    rm -rf "${CONSOLE_DATADIR}"
  else
    skip 'phase 73m: harbor console subcommand absent'
  fi

  # 3. D-091 binding rule: harbor dev MUST NOT advertise console-serving.
  if ./bin/harbor dev --help 2>&1 | grep -qiE -- '--console'; then
    fail 'phase 73m: harbor dev --help advertises a console-serving flag (D-091 violation)'
  else
    ok 'phase 73m: harbor dev --help does NOT advertise a console-serving flag (D-091 honoured)'
  fi
else
  skip 'phase 73m: ./bin/harbor not built — harbor console smoke deferred'
fi

smoke_summary
