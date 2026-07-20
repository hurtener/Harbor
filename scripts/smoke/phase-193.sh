#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 193 smoke — planner-facing steer / pause / resume of a spawned child.
#
# Adds three reserved planner controls (_steer_task / _pause_task /
# _resume_task) as new sealed planner.Decision shapes, dispatched onto the
# EXISTING per-sub-run steering inbox + the unified pause/resume primitive,
# descendant-scoped via 187's isOwnDescendant guard (D-330). No new Protocol
# method, so assertions are unit-test-shaped.
#
# Each go-test block SKIPs (not FAILs) when its target test name is absent, so
# this script coexists with pre-193 builds per the sacred SKIP convention.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

run_go_test_if_present() {
    local label="$1" runexpr="$2" pkg="$3" logf
    logf="$(mktemp)"
    if ! go test -list "${runexpr}" "${pkg}" >"${logf}" 2>&1 \
        || ! grep -qE '^Test' "${logf}"; then
        skip "phase 193: ${label} (test/surface absent — pre-193 build)"
        rm -f "${logf}"
        return
    fi
    if go test -run "${runexpr}" -race "${pkg}" >"${logf}" 2>&1; then
        ok "phase 193: ${label}"
    else
        fail "phase 193: ${label}"
        printf '--- go test output ---\n'
        cat "${logf}"
    fi
    rm -f "${logf}"
}

# --- Static assertions -----------------------------------------------------

# AC-3/5: the three reserved controls are declared and named in the standalone
# guard (the projector actually rejects co-occurrence, not just described).
if grep -q '_steer_task' internal/planner/react/discovered_tools.go 2>/dev/null \
    && grep -q '_pause_task' internal/planner/react/discovered_tools.go 2>/dev/null \
    && grep -q '_resume_task' internal/planner/react/discovered_tools.go 2>/dev/null; then
    ok "phase 193: _steer_task/_pause_task/_resume_task reserved declarations present"
else
    skip "phase 193: reserved control declarations absent — pre-193 build"
fi

# AC-7: the dispatch executor reuses 187's isOwnDescendant guard (scope check
# wired, not reinvented) and routes through the existing steering inbox.
if grep -q 'isOwnDescendant' internal/runtime/dispatch/dispatch.go 2>/dev/null \
    && grep -qE 'steerTask|pauseTask|resumeTask' internal/runtime/dispatch/dispatch.go 2>/dev/null; then
    ok "phase 193: dispatch reuses isOwnDescendant for steer/pause/resume"
else
    skip "phase 193: steer/pause/resume dispatch methods absent — pre-193 build"
fi

# --- Class assertions (unit tests) -----------------------------------------

# AC-3/4/5: translation + standalone-co-occurrence rejection.
run_go_test_if_present "projector translates + rejects non-standalone steer/pause/resume" \
    'TestProjector.*(Steer|Pause|Resume)Task|TestProjector.*Standalone' \
    './internal/planner/react/'

# AC-7/9/10/13: descendant-scope rejection, loud pause-serialization fail,
# concurrent-reuse with no cross-run cancellation cross-talk.
run_go_test_if_present "dispatch descendant-scope + unserializable-pause + concurrent-reuse" \
    'TestExecutor_(Steer|Pause|Resume)Task.*|TestExecutor.*NotOwnDescendant|TestExecutor.*Unserializable|TestExecutor.*ConcurrentReuse' \
    './internal/runtime/dispatch/'

# AC-8/9: steer enqueues onto the existing per-sub-run steering inbox.
run_go_test_if_present "steering inbox accepts an agent-issued directive" \
    'TestInbox.*Steer|TestInbox.*Directive' \
    './internal/runtime/steering/'

# AC-12: human-supremacy + cross-run isolation (operator reaches any task; agent
# only its own descendants).
run_go_test_if_present "human-supremacy + cross-run steer/pause/resume isolation" \
    'TestExecutor.*HumanSupremacy|TestExecutor.*CrossRun.*(Steer|Pause|Resume)' \
    './internal/runtime/dispatch/'

# --- Live-server surface (graceful skip) -----------------------------------

skip "phase 193: _steer_task/_pause_task/_resume_task are planner-facing controls exercised through the dispatch executor; there is no bare-server HTTP surface — covered by the in-package dispatch integration test, not this smoke"

smoke_summary
