#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
# Phase 69 smoke — `harbor inspect-events` + `harbor inspect-runs`
# (Phase 69 / D-101).
#
# Both subcommands are SSE-client consumers of the Phase 60 Protocol
# event stream. The preflight harness boots `bin/harbor dev` against
# 127.0.0.1:${HARBOR_BIND} (ephemeral port by default — D-104) with
# HARBOR_DEV_ALLOW_MOCK=1; this smoke:
#
#   1. Runs the cmd/harbor package tests under -race (unit + golden
#      coverage for inspect-events + inspect-runs, plus the existing
#      Phase 63 suite).
#   2. Drives a `start` over the live Protocol REST surface (mints a
#      real task.spawned on the bus), then invokes the built
#      `bin/harbor inspect-events --follow=false` against the same
#      bind and asserts the CLI emits the event under --json.
#   3. Invokes `bin/harbor inspect-runs --json` and asserts the run
#      surfaces as a row.
#
# 404/405/501 → SKIP per the standard convention. The smoke runs only
# the assertions whose surfaces are live; on a build that pre-dates
# Phase 60 / Phase 64 the inspect calls skip cleanly.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

BIN="${ROOT}/bin/harbor"

# 1. cmd/harbor package tests cover all unit + golden assertions.
test_log=$(mktemp)
if go test -race -count=1 -timeout 90s ./cmd/harbor/... >"${test_log}" 2>&1; then
    ok 'phase 69: cmd/harbor unit + golden tests pass under -race'
    rm -f "${test_log}"
else
    fail 'phase 69: cmd/harbor tests failed (run: go test -race ./cmd/harbor/...)'
    echo "    --- go test output (tail 60 lines) ---"
    tail -60 "${test_log}" | sed 's/^/    /'
    echo "    --- end ---"
    rm -f "${test_log}"
fi

# 2. Built-binary checks. If preflight skipped the build, skip cleanly.
if [[ ! -x "${BIN}" ]]; then
    skip 'phase 69: bin/harbor not built (preflight build step skipped)'
    smoke_summary
    exit 0
fi

# 3. The wire-side assertions need the dev server up. If preflight did
# not boot it (no harbor dev surface yet), skip.
if ! skip_if_404 "$(api_url /healthz)" 'phase 69: dev server reachable for inspect-* live test'; then
    smoke_summary
    exit 0
fi

# Resolve the dev token. The preflight harness writes the server log
# to ${HARBOR_DATA_DIR}/server.log; the dev cmd prints HARBOR_DEV_TOKEN
# under that prefix.
if [[ -z "${HARBOR_DATA_DIR:-}" ]] || [[ ! -f "${HARBOR_DATA_DIR}/server.log" ]]; then
    skip 'phase 69: HARBOR_DATA_DIR/server.log absent — cannot mint dev token for inspect-* wire test'
    smoke_summary
    exit 0
fi
TOKEN="$(grep -m1 '^HARBOR_DEV_TOKEN=' "${HARBOR_DATA_DIR}/server.log" 2>/dev/null | sed 's/^HARBOR_DEV_TOKEN=//')"
if [[ -z "${TOKEN}" ]]; then
    skip 'phase 69: HARBOR_DEV_TOKEN not found in server.log — skipping wire-side inspect tests'
    smoke_summary
    exit 0
fi
export HARBOR_TOKEN="${TOKEN}"

# 4. Drive a `start` so the bus carries at least one task.spawned event
# the inspect-events snapshot can observe. The control surface returns
# the task id which doubles as the run id in inspect-runs.
START_BODY='{"identity":{"tenant":"dev","user":"dev","session":"dev"},"query":"phase-69 smoke"}'
START_RESP="$(mktemp)"
start_status=$(curl -s -o "${START_RESP}" -w '%{http_code}' \
    --max-time 5 \
    -X POST \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    --data "${START_BODY}" \
    "$(api_url /v1/control/start)" || echo "000")
case "${start_status}" in
    200)
        ok "phase 69: live /v1/control/start returned 200 — bus seeded for inspect-events"
        ;;
    404|405|501)
        skip "phase 69: /v1/control/start not implemented (${start_status}) — wire-side tests skipped"
        smoke_summary
        exit 0
        ;;
    *)
        fail "phase 69: /v1/control/start status = ${start_status}, want 200 (body: $(cat "${START_RESP}" 2>/dev/null || echo '(empty)'))"
        rm -f "${START_RESP}"
        smoke_summary
        exit 1
        ;;
esac
TASK_ID=""
if command -v jq >/dev/null 2>&1; then
    TASK_ID=$(jq -r '.task_id // empty' "${START_RESP}" 2>/dev/null || true)
fi
rm -f "${START_RESP}"

# 5. inspect-events --follow=false --json: snapshot the SSE stream
# and assert a JSON line carrying type=task.spawned arrives. Bound
# the timeout shorter than the default 20s snapshotIdleTimeout —
# preflight tail must finish promptly.
EVENTS_LOG="$(mktemp)"
events_status=0
"${BIN}" inspect-events \
    --bind "${HARBOR_BIND:-127.0.0.1:18080}" \
    --tenant dev --user dev --session dev \
    --type task.spawned \
    --since 0 \
    --follow=false \
    --json \
    > "${EVENTS_LOG}" 2>&1 || events_status=$?

if [[ "${events_status}" -ne 0 ]]; then
    fail "phase 69: inspect-events exited ${events_status}"
    echo "    --- inspect-events output ---"
    head -10 "${EVENTS_LOG}" | sed 's/^/    /'
    echo "    --- end ---"
elif grep -q '"type":"task.spawned"' "${EVENTS_LOG}" 2>/dev/null; then
    ok "phase 69: harbor inspect-events --json carries the live task.spawned event"
else
    fail "phase 69: inspect-events output did not include task.spawned"
    echo "    --- inspect-events output ---"
    head -10 "${EVENTS_LOG}" | sed 's/^/    /'
    echo "    --- end ---"
fi
rm -f "${EVENTS_LOG}"

# 6. inspect-runs --json (list mode): asserts the live run is visible.
RUNS_LOG="$(mktemp)"
runs_status=0
"${BIN}" inspect-runs \
    --bind "${HARBOR_BIND:-127.0.0.1:18080}" \
    --tenant dev --user dev --session dev \
    --since 0 \
    --json \
    > "${RUNS_LOG}" 2>&1 || runs_status=$?

if [[ "${runs_status}" -ne 0 ]]; then
    fail "phase 69: inspect-runs (list) exited ${runs_status}"
    head -5 "${RUNS_LOG}" | sed 's/^/    /'
elif command -v jq >/dev/null 2>&1; then
    # Expect a JSON array with ≥1 row.
    count=$(jq 'length' "${RUNS_LOG}" 2>/dev/null || echo 0)
    if [[ "${count}" -gt 0 ]]; then
        ok "phase 69: harbor inspect-runs (list) emitted ${count} run(s)"
    else
        fail "phase 69: inspect-runs returned empty array — bus should carry ≥1 run"
        head -5 "${RUNS_LOG}" | sed 's/^/    /'
    fi
else
    # No jq — substring fallback.
    if grep -q '"run_id"' "${RUNS_LOG}" 2>/dev/null; then
        ok "phase 69: harbor inspect-runs (list) contains a run_id (substring check; jq absent)"
    else
        fail "phase 69: inspect-runs (list) body missing run_id"
        head -5 "${RUNS_LOG}" | sed 's/^/    /'
    fi
fi
rm -f "${RUNS_LOG}"

# 7. inspect-runs <task-id> --json (trajectory mode): if the start
# response yielded a task id, replay it and assert ≥1 step.
if [[ -n "${TASK_ID}" ]]; then
    TRAJ_LOG="$(mktemp)"
    traj_status=0
    "${BIN}" inspect-runs "${TASK_ID}" \
        --bind "${HARBOR_BIND:-127.0.0.1:18080}" \
        --tenant dev --user dev --session dev \
        --since 0 \
        --json \
        > "${TRAJ_LOG}" 2>&1 || traj_status=$?
    if [[ "${traj_status}" -ne 0 ]]; then
        fail "phase 69: inspect-runs ${TASK_ID} exited ${traj_status}"
        head -5 "${TRAJ_LOG}" | sed 's/^/    /'
    elif command -v jq >/dev/null 2>&1; then
        steps=$(jq '.steps | length' "${TRAJ_LOG}" 2>/dev/null || echo 0)
        if [[ "${steps}" -gt 0 ]]; then
            ok "phase 69: harbor inspect-runs ${TASK_ID} returned ${steps} step(s)"
        else
            fail "phase 69: inspect-runs ${TASK_ID} returned zero steps"
            head -5 "${TRAJ_LOG}" | sed 's/^/    /'
        fi
    else
        if grep -q '"steps"' "${TRAJ_LOG}" 2>/dev/null; then
            ok "phase 69: harbor inspect-runs ${TASK_ID} contains steps array (substring check; jq absent)"
        else
            fail "phase 69: inspect-runs trajectory body missing steps"
        fi
    fi
    rm -f "${TRAJ_LOG}"
fi

smoke_summary
