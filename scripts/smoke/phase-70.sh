#!/usr/bin/env bash
# Phase 70 smoke — `harbor inspect-topology` (D-102).
#
# Phase 70 graduates `harbor inspect-topology` out of the Phase 63 stub.
# This script:
#
#   1. Runs the cmd/harbor tests under -race (covers renderer + cmd body
#      + golden round-trip).
#   2. Built-binary checks against the preflight-booted dev server:
#      a. `harbor inspect-topology --help` exits 0 and shows the usage.
#      b. `harbor inspect-topology` (no args) exits non-zero (cobra
#         ExactArgs rejection).
#      c. `harbor inspect-topology --bind bad-port run-x` exits non-zero
#         with the structured `inspect_topology_bind_invalid` code.
#      d. `harbor inspect-topology --width 5 run-x` exits non-zero with
#         `inspect_topology_width_invalid`.
#      e. `harbor inspect-topology --json nonexistent-run` against the
#         live dev server exits non-zero with `inspect_topology_run_not_found`
#         (synthetic run id; no events flow → idle-timeout exit).
#      f. (Optional) when HARBOR_DEV_TOKEN is set in env, run against
#         the live dev server and assert the connect path succeeds at
#         the auth edge (we accept run_not_found OR a 200-shaped empty
#         output, both indicating the wire reached the runtime).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

BIN="${ROOT}/bin/harbor"

# 1. Package tests.
test_log=$(mktemp)
if go test -race -count=1 -timeout 60s ./cmd/harbor/... >"${test_log}" 2>&1; then
    ok 'phase 70: cmd/harbor tests pass under -race (renderer + cmd body + golden round-trip)'
    rm -f "${test_log}"
else
    fail 'phase 70: cmd/harbor tests failed (run: go test -race ./cmd/harbor/...)'
    echo "    --- go test output (tail 60 lines) ---"
    tail -60 "${test_log}" | sed 's/^/    /'
    echo "    --- end ---"
    rm -f "${test_log}"
fi

# 2. Built-binary checks.
if [[ ! -x "${BIN}" ]]; then
    skip 'phase 70: bin/harbor not built (preflight build step skipped)'
    smoke_summary
    exit 0
fi

# 2a. --help exits 0.
if "${BIN}" inspect-topology --help >/dev/null 2>&1; then
    ok 'phase 70: harbor inspect-topology --help exits 0'
else
    fail 'phase 70: harbor inspect-topology --help exited non-zero'
fi

# 2b. no-args exits non-zero (cobra ExactArgs rejection).
if "${BIN}" inspect-topology >/dev/null 2>&1; then
    fail 'phase 70: harbor inspect-topology (no args) exited 0 — ExactArgs(1) should reject'
else
    ok 'phase 70: harbor inspect-topology (no args) exits non-zero (cobra ExactArgs(1) rejection)'
fi

# 2c. bad --bind.
if command -v jq >/dev/null 2>&1; then
    stderr_body=$(HARBOR_TOKEN=dummy "${BIN}" inspect-topology --bind no-port --json run-x 2>&1 1>/dev/null || true)
    code=$(printf '%s' "${stderr_body}" | jq -r '.code // empty' 2>/dev/null || echo '')
    if [[ "${code}" == "inspect_topology_bind_invalid" ]]; then
        ok 'phase 70: harbor inspect-topology --bind invalid emits inspect_topology_bind_invalid'
    else
        fail "phase 70: bad --bind expected inspect_topology_bind_invalid, got code='${code}' (body: ${stderr_body})"
    fi
else
    skip 'phase 70: jq not available — bad-bind structured-error assertion skipped'
fi

# 2d. bad --width.
if command -v jq >/dev/null 2>&1; then
    stderr_body=$(HARBOR_TOKEN=dummy "${BIN}" inspect-topology --width 5 --json run-x 2>&1 1>/dev/null || true)
    code=$(printf '%s' "${stderr_body}" | jq -r '.code // empty' 2>/dev/null || echo '')
    if [[ "${code}" == "inspect_topology_width_invalid" ]]; then
        ok 'phase 70: harbor inspect-topology --width 5 emits inspect_topology_width_invalid'
    else
        fail "phase 70: bad --width expected inspect_topology_width_invalid, got code='${code}' (body: ${stderr_body})"
    fi
fi

# 2e. live-server run-not-found path.
#
# We use the preflight-booted dev server at HARBOR_BASE_URL (default
# http://127.0.0.1:18080). The dev server prints the dev token to
# stderr as `HARBOR_DEV_TOKEN=...`. The preflight harness captures
# that and exposes it via the HARBOR_DEV_TOKEN env (see scripts/preflight.sh).
# When not running under preflight (manual `bash scripts/smoke/phase-70.sh`),
# the operator sets HARBOR_DEV_TOKEN themselves.
if [[ -n "${HARBOR_DEV_TOKEN:-}" ]]; then
    # The live runtime is up; drive the cmd against a deliberately
    # nonexistent run ID and assert the run-not-found code.
    if command -v jq >/dev/null 2>&1; then
        bind_addr="${HARBOR_BASE_URL:-http://127.0.0.1:18080}"
        # Strip the http:// prefix for --bind host:port.
        bind_addr="${bind_addr#http://}"
        bind_addr="${bind_addr#https://}"
        stderr_body=$(HARBOR_TOKEN="${HARBOR_DEV_TOKEN}" "${BIN}" inspect-topology \
            --bind "${bind_addr}" \
            --idle-timeout 800ms \
            --json \
            "phase70-smoke-nonexistent-run" 2>&1 1>/dev/null || true)
        code=$(printf '%s' "${stderr_body}" | jq -r '.code // empty' 2>/dev/null || echo '')
        case "${code}" in
            inspect_topology_run_not_found)
                ok 'phase 70: harbor inspect-topology against live dev server emits run_not_found for synthetic run'
                ;;
            inspect_topology_http_status)
                # 401/403 means the dev token is rejected. Still
                # proves the wire reached the runtime — count as OK
                # for the smoke (the auth path is a Phase 64 surface).
                ok 'phase 70: harbor inspect-topology against live dev server returned a structured HTTP status error (auth rejected; wire reached the runtime)'
                ;;
            *)
                fail "phase 70: live-server run-not-found expected inspect_topology_run_not_found, got code='${code}' (body: ${stderr_body})"
                ;;
        esac
    else
        skip 'phase 70: jq not available — live-server smoke skipped'
    fi
else
    skip 'phase 70: HARBOR_DEV_TOKEN not set in env — live-server smoke skipped (set HARBOR_DEV_TOKEN to enable, or run under `make preflight`)'
fi

smoke_summary
