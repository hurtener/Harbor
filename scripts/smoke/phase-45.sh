#!/usr/bin/env bash
# Phase 45 smoke — Reference ReAct planner (RFC §6.2; master plan
# Phase 45 detail block; D-051).
#
# Phase 45 ships Harbor's first concrete Planner under
# `internal/planner/react/`. The planner drives an LLM-only step
# loop: build prompt from RunContext → llm.LLMClient.Complete →
# repair.RepairLoop.Run (salvage → schema repair → graceful failure
# → multi-action salvage) → map to Decision. JSON-only action format
# (`{"tool":..., "args":..., "reasoning":...}` or
# `{"tool":"_finish","args":{"answer":...}}`), single tool call per
# step (multi-action salvage collapses to first), WakePush wake
# declaration (D-032), MaxSteps circuit breaker emitting
# planner.max_steps_exceeded — the fail-loudly surface that makes
# the breaker NOT silent (§13).
#
# This is a code-only phase; no protocol surface lands until
# Phase 60+.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Run the react package tests under -race. Covers all unit tests +
# integration + D-025 concurrent-reuse + conformance.
if go test -race -count=1 -timeout 180s ./internal/planner/react/... >/dev/null 2>&1; then
    ok 'phase 45: internal/planner/react tests pass under -race (3-step scenario + max-steps breaker + D-025 + conformance + integration)'
else
    fail 'phase 45: react tests failed (run `go test -race ./internal/planner/react/...` for detail)'
fi

# Static guard: the WakePush declaration must be wired (D-032; Phase
# 45 master-plan detail block — "ReAct ships the `push` wake mode").
REACT_FILE="internal/planner/react/react.go"
if [[ ! -f "${REACT_FILE}" ]]; then
    fail "phase 45: ${REACT_FILE} missing"
else
    if grep -q 'WakePush' "${REACT_FILE}"; then
        ok 'phase 45: ReActPlanner declares planner.WakePush wake-on-resolution mode (D-032)'
    else
        fail 'phase 45: ReActPlanner missing WakePush declaration in react.go (D-032 — Phase 45 spec)'
    fi
fi

# Event-registry assertion — the fail-loudly surface for the
# MaxSteps circuit breaker.
if grep -q 'EventTypePlannerMaxStepsExceeded' internal/planner/events.go 2>/dev/null; then
    ok 'phase 45: planner.max_steps_exceeded event type registered (fail-loudly emit surface for MaxSteps breaker)'
else
    fail 'phase 45: planner.max_steps_exceeded event type missing from internal/planner/events.go (silent-degradation risk; §13)'
fi

# §13 import-graph guard for the react package — no runtime imports.
if grep -rIn --include='*.go' 'github.com/hurtener/Harbor/internal/runtime' internal/planner/react/ 2>/dev/null | grep -q .; then
    fail 'phase 45: internal/planner/react/ imports internal/runtime/... — violates Phase 42 import-graph contract (§13)'
else
    ok 'phase 45: internal/planner/react/ does not import internal/runtime/... (Phase 42 import-graph contract preserved)'
fi

# Static guard: no two parallel retry-feedback implementations. The
# react package consumes llm.LLMClient (composed with Phase 36 retry
# at the registry edge); it does NOT embed a copy of the retry loop.
if grep -rIn --include='*.go' '"github.com/hurtener/Harbor/internal/llm/retry"' internal/planner/react/ 2>/dev/null | grep -q .; then
    fail 'phase 45: internal/planner/react/ imports internal/llm/retry — composition stays at the registry edge (D-043 + D-050 + D-051)'
else
    ok 'phase 45: internal/planner/react/ does not import internal/llm/retry (composition stays clean — D-043 + D-051)'
fi

# Static guard: the planner consumes the Phase 44 repair loop. A
# react package that re-implemented schema repair would be a §13
# two-parallel-implementations violation.
if grep -rIn --include='*.go' '"github.com/hurtener/Harbor/internal/planner/repair"' internal/planner/react/ 2>/dev/null | grep -q .; then
    ok 'phase 45: internal/planner/react/ consumes internal/planner/repair (no parallel repair implementation — §13 / D-050)'
else
    fail 'phase 45: internal/planner/react/ does NOT consume internal/planner/repair — Phase 44 schema repair must be reused, not duplicated (§13)'
fi

skip "phase 45: react package is a planner concrete with no protocol surface (lands in Phase 60+ via the planner-step executor)"

smoke_summary
