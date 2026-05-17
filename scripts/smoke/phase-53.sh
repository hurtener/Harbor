#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 53 smoke — Steering wiring (9 control events) (RFC §6.3;
# master-plan Phase 53 detail block; D-071).
#
# Phase 53 ships `internal/runtime/steering.RunLoop` — the per-run
# planner-step loop that drains the per-run steering inbox between
# steps, applies the nine control events' side effects, and routes a
# planner's RequestPause decision through the unified Phase 50
# pauseresume.Coordinator. It is the §13 first consumer of BOTH the
# Phase 50 Coordinator and the Phase 52 inbox/taxonomy.
#
# Phase 53 is a code-only runtime-wiring phase: the steering Protocol
# endpoints (task.cancel / task.pause / task.approve / ...) land in
# Phase 54. So the HTTP/Protocol assertions skip with a reason per the
# 404/405/501 -> SKIP convention.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

ST_PKG="internal/runtime/steering"

# Run the steering package tests under -race. Covers unit (the RunLoop
# control flow, the nine per-control-type apply paths, the per-session
# capped control-history ring, Inbox.WaitForEvent, the pause-routing
# round-trip against a stub Coordinator, the control.received /
# control.applied lifecycle events) + the D-025 concurrent-reuse test
# (N>=100 Run invocations against one shared RunLoop).
if go test -race -count=1 -timeout 180s ./${ST_PKG}/... >/dev/null 2>&1; then
    ok 'phase 53: internal/runtime/steering tests pass under -race (RunLoop control flow + nine-event apply paths + control-history cap + WaitForEvent + pause-routing + D-025 concurrent-reuse)'
else
    fail 'phase 53: steering tests failed (run `go test -race ./internal/runtime/steering/...` for detail)'
fi

# Run the Phase 53 integration test — the nine-event matrix + the §13
# pause round-trip through the REAL pauseresume.Coordinator + the
# drain-between-steps invariant + the concurrency-mid-step stress.
# Real drivers on every seam (real Registry, real Coordinator over a
# real in-mem StateStore checkpoint store, real EventBus, real
# TaskRegistry, real deterministic planner).
if go test -race -count=1 -timeout 180s -run 'TestE2E_Phase53' ./test/integration/... >/dev/null 2>&1; then
    ok 'phase 53: steering-wiring integration test passes under -race (9-event matrix + §13 pause-Coordinator round-trip + drain-between-steps invariant + concurrency-mid-step)'
else
    fail 'phase 53: steering-wiring integration test failed (run `go test -race -run TestE2E_Phase53 ./test/integration/...` for detail)'
fi

# Static guard: the RunLoop must exist and import the unified
# pause/resume primitive — the §13 consumer wires the REAL
# pauseresume.Coordinator; it does NOT mint a parallel pause
# coordinator (CLAUDE.md §7 rule 4, D-070 §5).
RL_FILE="${ST_PKG}/runloop.go"
if [[ ! -f "${RL_FILE}" ]]; then
    fail "phase 53: ${RL_FILE} missing — the steering-wiring RunLoop is not implemented"
elif ! grep -q 'runtime/pauseresume' "${RL_FILE}"; then
    fail 'phase 53: internal/runtime/steering/runloop.go does not import internal/runtime/pauseresume — the §13 consumer must wire the unified Coordinator'
else
    ok 'phase 53: RunLoop wires the unified pauseresume.Coordinator (the §13 first consumer; no parallel pause coordinator — CLAUDE.md §7 rule 4)'
fi

# §7 rule 4 guard: still no parallel pause Coordinator interface under
# the steering package after Phase 53's wiring landed.
if grep -rIn --include='*.go' 'type .*Coordinator .*interface' "${ST_PKG}/" 2>/dev/null | grep -q .; then
    fail 'phase 53: internal/runtime/steering declares a pause Coordinator — PAUSE/RESUME/APPROVE/REJECT must converge on the unified pauseresume primitive (CLAUDE.md §7 rule 4)'
else
    ok 'phase 53: no parallel pause coordinator under internal/runtime/steering (pause-family controls converge on the unified pauseresume primitive)'
fi

# Static guard: the two Phase 53 lifecycle events are registered.
EV_FILE="${ST_PKG}/events.go"
if grep -q 'control.received' "${EV_FILE}" && grep -q 'control.applied' "${EV_FILE}"; then
    ok 'phase 53: steering events.go registers the control.received + control.applied lifecycle events (brief 02 §3)'
else
    fail 'phase 53: events.go is missing the control.received / control.applied lifecycle event registrations'
fi

# Import-graph guard: the steering package must NOT import the Console
# — the Runtime never imports Console code (CLAUDE.md §13).
if grep -rIn --include='*.go' '"github.com/hurtener/Harbor/web/console' "${ST_PKG}/" 2>/dev/null | grep -q .; then
    fail 'phase 53: internal/runtime/steering imports the Console — the Runtime never imports Console code (CLAUDE.md §13)'
else
    ok 'phase 53: internal/runtime/steering does not import the Console (Runtime/Console boundary preserved)'
fi

# §13 / §4.4 guard: the RunLoop is an in-process runtime mechanism with
# no plausible alternate backend — Phase 53 must NOT mint a
# driver-registry tree (the same call D-070 §5 made for the inbox).
if [[ -d "${ST_PKG}/drivers" ]]; then
    fail 'phase 53: internal/runtime/steering/drivers/ exists — the RunLoop is an in-process primitive with no alternate backend (no §4.4 seam needed — D-071)'
else
    ok 'phase 53: no driver-registry tree under internal/runtime/steering (the RunLoop is an in-process primitive — D-071)'
fi

# Phase 53 ships no Protocol/HTTP surface — the steering Protocol
# endpoints land in Phase 54. Skip per the 404/405/501 -> SKIP
# convention.
skip "phase 53: the steering RunLoop is a code-only runtime-wiring primitive; the steering Protocol endpoints (task.cancel / task.pause / task.approve / ...) land in Phase 54"

smoke_summary
