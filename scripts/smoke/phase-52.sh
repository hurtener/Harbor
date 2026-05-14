#!/usr/bin/env bash
# Phase 52 smoke — Steering inbox + control taxonomy (RFC §6.3;
# master-plan Phase 52 detail block; D-070).
#
# Phase 52 ships `internal/runtime/steering` — the per-run steering
# inbox + the nine-event control taxonomy + the Protocol-edge
# validation / sanitisation + the per-event scope checks. It is a
# code-only runtime-primitive phase: the steering Protocol endpoints
# (task.cancel / task.pause / task.inject_context / ...) land in
# Phase 54, and the engine run-loop wiring lands in Phase 53. So the
# HTTP/Protocol assertions skip with a reason per the
# 404/405/501 → SKIP convention.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

ST_PKG="internal/runtime/steering"

# Run the steering package tests under -race. Covers unit (the
# nine-type taxonomy, every payload bound — depth / keys / list /
# string / total, every per-event scope check, the per-run Inbox
# enqueue/drain, the Registry lifecycle) + the D-025 concurrent-reuse
# test (N≥100 against one shared Registry + one shared Inbox).
if go test -race -count=1 -timeout 180s ./${ST_PKG}/... >/dev/null 2>&1; then
    ok 'phase 52: internal/runtime/steering tests pass under -race (taxonomy + payload bounds + per-event scope + inbox/registry + D-025 concurrent-reuse)'
else
    fail 'phase 52: steering tests failed (run `go test -race ./internal/runtime/steering/...` for detail)'
fi

# Run the Phase 52 auth-scope-per-event integration test (real
# events.EventBus + real patterns redactor on the seam).
if go test -race -count=1 -timeout 180s -run 'TestE2E_Phase52' ./test/integration/... >/dev/null 2>&1; then
    ok 'phase 52: steering integration test passes under -race (auth-scope-per-event + cross-tenant + payload-bounds failure mode + per-run isolation + concurrency stress against a real EventBus)'
else
    fail 'phase 52: steering integration test failed (run `go test -race -run TestE2E_Phase52 ./test/integration/...` for detail)'
fi

# Static guard: the nine-type control taxonomy must be present
# verbatim (RFC §6.3 — Settled).
TAX_FILE="${ST_PKG}/taxonomy.go"
if [[ ! -f "${TAX_FILE}" ]]; then
    fail "phase 52: ${TAX_FILE} missing"
else
    missing_types=""
    for ct in INJECT_CONTEXT REDIRECT CANCEL PRIORITIZE PAUSE RESUME APPROVE REJECT USER_MESSAGE; do
        if ! grep -q "\"${ct}\"" "${TAX_FILE}"; then
            missing_types="${missing_types} ${ct}"
        fi
    done
    if [[ -n "${missing_types}" ]]; then
        fail "phase 52: control taxonomy missing types:${missing_types}"
    else
        ok 'phase 52: steering taxonomy declares all nine RFC §6.3 control types (INJECT_CONTEXT / REDIRECT / CANCEL / PRIORITIZE / PAUSE / RESUME / APPROVE / REJECT / USER_MESSAGE)'
    fi
fi

# Static guard: the RFC §6.3 payload bounds must be present as named
# constants (depth ≤ 6, ≤ 64 keys, ≤ 50 list items, ≤ 4096
# chars/string, ≤ 16 KiB total).
VAL_FILE="${ST_PKG}/validate.go"
if [[ ! -f "${VAL_FILE}" ]]; then
    fail "phase 52: ${VAL_FILE} missing"
else
    missing_bounds=""
    for bound in MaxPayloadDepth MaxPayloadKeys MaxPayloadListItems MaxPayloadStringLen MaxPayloadTotalBytes; do
        if ! grep -q "${bound}" "${VAL_FILE}"; then
            missing_bounds="${missing_bounds} ${bound}"
        fi
    done
    if [[ -n "${missing_bounds}" ]]; then
        fail "phase 52: validation bounds missing:${missing_bounds}"
    else
        ok 'phase 52: steering validate.go declares all five RFC §6.3 payload bounds (depth / keys / list / string / total)'
    fi
fi

# Import-graph guard: the steering package must NOT import the
# Console — the Runtime never imports Console code (CLAUDE.md §13).
if grep -rIn --include='*.go' '"github.com/hurtener/Harbor/web/console' "${ST_PKG}/" 2>/dev/null | grep -q .; then
    fail 'phase 52: internal/runtime/steering imports the Console — the Runtime never imports Console code (CLAUDE.md §13)'
else
    ok 'phase 52: internal/runtime/steering does not import the Console (Runtime/Console boundary preserved)'
fi

# §7 rule 4 guard: PAUSE/RESUME/APPROVE/REJECT converge on the
# unified pause/resume primitive — Phase 52 must NOT mint a parallel
# pause coordinator. The steering package ships the taxonomy + inbox;
# the pause WIRING is Phase 53, onto internal/runtime/pauseresume.
if grep -rIn --include='*.go' 'type .*Coordinator .*interface' "${ST_PKG}/" 2>/dev/null | grep -q .; then
    fail 'phase 52: internal/runtime/steering declares a pause Coordinator — PAUSE/RESUME/APPROVE/REJECT must converge on the unified pauseresume primitive (CLAUDE.md §7 rule 4)'
else
    ok 'phase 52: no parallel pause coordinator under internal/runtime/steering (pause-family controls converge on the unified pauseresume primitive — CLAUDE.md §7 rule 4)'
fi

# §13 / §4.4 guard: an in-process per-run inbox has no plausible
# alternate backend — Phase 52 must NOT mint a driver-registry tree.
if [[ -d "${ST_PKG}/drivers" ]]; then
    fail 'phase 52: internal/runtime/steering/drivers/ exists — the per-run inbox is an in-process primitive with no alternate backend (no §4.4 seam needed — D-070)'
else
    ok 'phase 52: no driver-registry tree under internal/runtime/steering (the per-run inbox is an in-process primitive — D-070)'
fi

# Phase 52 ships no Protocol/HTTP surface — the steering Protocol
# endpoints land in Phase 54, the engine run-loop wiring in Phase 53.
# Skip per the 404/405/501 → SKIP convention.
skip "phase 52: the steering inbox + control taxonomy is a code-only runtime primitive; the steering Protocol endpoints land in Phase 54 and the engine run-loop wiring in Phase 53"

smoke_summary
