#!/usr/bin/env bash
# Phase 44 smoke — schema repair pipeline (RFC §6.2; master plan
# Phase 44 detail block; D-050).
#
# Phase 44 ships the salvage → schema repair → graceful failure →
# multi-action salvage ladder under `internal/planner/repair/`. The
# loop is configurable per-concrete via three knobs:
#
#   - arg_fill_enabled
#   - repair_attempts
#   - max_consecutive_arg_failures
#
# On exhaustion the loop returns Finish{Reason: NoPath, Followup: true}
# and emits `planner.repair_exhausted` (the fail-loudly surface that
# makes graceful failure NOT silent — §13 / D-050).
#
# This is a code-only phase; no protocol surface lands until Phase 60+.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Run the repair package tests under -race. Covers salvage,
# schema-repair, graceful-failure, multi-action salvage, parser unit
# tests, integration (malformed-then-valid mock LLM), and D-025
# concurrent-reuse.
if go test -race -count=1 -timeout 180s ./internal/planner/repair/... >/dev/null 2>&1; then
    ok 'phase 44: internal/planner/repair tests pass under -race (salvage + repair + graceful + multi-action + D-025)'
else
    fail 'phase 44: repair tests failed (run `go test -race ./internal/planner/repair/...` for detail)'
fi

# Static guard: the three config-knob names must be wired into the loop.
REPAIR_FILE="internal/planner/repair/repair.go"
if [[ ! -f "${REPAIR_FILE}" ]]; then
    fail "phase 44: ${REPAIR_FILE} missing"
else
    missing=()
    for knob in ArgFillEnabled RepairAttempts MaxConsecutiveArgFailures; do
        if ! grep -q "${knob}" "${REPAIR_FILE}"; then
            missing+=("${knob}")
        fi
    done
    if [[ ${#missing[@]} -gt 0 ]]; then
        fail "phase 44: repair Config missing knob(s): ${missing[*]} (RFC §6.2)"
    else
        ok 'phase 44: repair Config wires ArgFillEnabled + RepairAttempts + MaxConsecutiveArgFailures (RFC §6.2)'
    fi
fi

# Event-registry assertion — the fail-loudly surface.
if grep -q 'EventTypePlannerRepairExhausted' internal/planner/events.go 2>/dev/null; then
    ok 'phase 44: planner.repair_exhausted event type registered (fail-loudly emit surface for graceful failure)'
else
    fail 'phase 44: planner.repair_exhausted event type missing from internal/planner/events.go (silent-degradation risk; §13)'
fi

# §13 import-graph guard for the repair package — no runtime imports.
if grep -rIn --include='*.go' --exclude='*_test.go' 'github.com/hurtener/Harbor/internal/runtime' internal/planner/repair/ 2>/dev/null | grep -q .; then
    fail 'phase 44: internal/planner/repair/ imports internal/runtime/... — violates Phase 42 import-graph contract (§13)'
else
    ok 'phase 44: internal/planner/repair/ does not import internal/runtime/... (Phase 42 import-graph contract preserved)'
fi

# Static guard: no two parallel retry-feedback implementations.
# The repair package consumes the LLM-client interface (which the
# binary composes with the Phase 36 retry wrapper); it does NOT
# embed a copy of the retry loop.
if grep -rIn --include='*.go' --exclude='*_test.go' '"github.com/hurtener/Harbor/internal/llm/retry"' internal/planner/repair/ 2>/dev/null | grep -q .; then
    fail 'phase 44: internal/planner/repair/ imports internal/llm/retry — repair is OUTSIDE the LLM call, retry stays composed at registry edge (D-050)'
else
    ok 'phase 44: internal/planner/repair/ does not import internal/llm/retry (composition stays clean — D-050)'
fi

skip "phase 44: repair package is a planner-side utility with no protocol surface (lands in Phase 60+ via the planner-step executor)"

smoke_summary
