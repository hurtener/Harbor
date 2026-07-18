#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 186 smoke — Batch executor: heterogeneous dispatch, auto-grouping,
# ordered observations, structural whole-batch rejections, and the
# hard-cancel-hook seam this phase closes.
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.
#
# This is a unit-test-class smoke (source/build/test assertions, not a
# live server) — the Batch executor has no HTTP surface. It mirrors
# phase-185's static/unit pattern.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

DISPATCH="internal/runtime/dispatch/dispatch.go"
ASSEMBLE="internal/runtime/assemble/assemble.go"
CONFIG="internal/config/config.go"

# ----------------------------------------------------------------------------
# 0. Not-yet-built guard: skip the whole surface if the Batch dispatch case
#    isn't wired yet (the 404-equivalent for a static smoke).
# ----------------------------------------------------------------------------
if ! grep -qE 'case planner\.Batch:' "${DISPATCH}"; then
    skip 'phase 186: Batch dispatch case not yet wired — surface not built'
    smoke_summary
    exit 0
fi

# ----------------------------------------------------------------------------
# 1. ExecuteDecision dispatches planner.Batch through a batch method.
# ----------------------------------------------------------------------------
if grep -qE 'case planner\.Batch:' "${DISPATCH}" \
    && grep -qE 'func \(e \*toolExecutor\) batch\(' "${DISPATCH}"; then
    ok 'phase 186: ExecuteDecision routes planner.Batch to the batch method'
else
    fail 'phase 186: Batch dispatch case or batch method missing'
fi

# ----------------------------------------------------------------------------
# 2. The tool half dispatches through the SAME parallel.Executor path in
#    NON-ATOMIC mode (D-169 parity).
# ----------------------------------------------------------------------------
if grep -qE 'e\.parallel\.Execute\(ctx,' "${DISPATCH}" \
    && grep -qE 'parallel\.WithNonAtomicSetup\(\)' "${DISPATCH}"; then
    ok 'phase 186: Batch tool half reuses parallel.Executor in non-atomic mode'
else
    fail 'phase 186: Batch tool half does not reuse the non-atomic parallel path'
fi

# ----------------------------------------------------------------------------
# 3. The max_batch_spawns breadth cap knob + resolver + default exist.
# ----------------------------------------------------------------------------
if grep -qE 'MaxBatchSpawns +int' "${CONFIG}" \
    && grep -qE 'func \(p PlannerConfig\) BatchSpawnCap\(\)' "${CONFIG}" \
    && grep -qE 'DefaultMaxBatchSpawns' "${CONFIG}"; then
    ok 'phase 186: planner.max_batch_spawns field + BatchSpawnCap resolver + default present'
else
    fail 'phase 186: max_batch_spawns config surface incomplete'
fi

# ----------------------------------------------------------------------------
# 4. WithMaxBatchSpawns option exists on the dispatch executor.
# ----------------------------------------------------------------------------
if grep -qE 'func WithMaxBatchSpawns\(' "${DISPATCH}"; then
    ok 'phase 186: dispatch.WithMaxBatchSpawns option present'
else
    fail 'phase 186: dispatch.WithMaxBatchSpawns option missing'
fi

# ----------------------------------------------------------------------------
# 5. BatchObservation call-id-keyed observation types exist.
# ----------------------------------------------------------------------------
if grep -qE 'type BatchObservation struct' internal/planner/batch_observation.go \
    && grep -qE 'type BatchSpawnObservation struct' internal/planner/batch_observation.go; then
    ok 'phase 186: BatchObservation + BatchSpawnObservation types present'
else
    fail 'phase 186: BatchObservation types missing'
fi

# ----------------------------------------------------------------------------
# 6. The assembler wires BOTH the breadth cap AND the hard-cancel hook
#    into the ONE production stack — the regression guard against the hook
#    silently going unwired again (its only production call site).
# ----------------------------------------------------------------------------
if grep -qE 'dispatch\.WithMaxBatchSpawns\(' "${ASSEMBLE}" \
    && grep -qE 'steering\.WithHardCancelHook\(' "${ASSEMBLE}"; then
    ok 'phase 186: assembler wires WithMaxBatchSpawns AND WithHardCancelHook'
else
    fail 'phase 186: assembler missing the batch-cap or hard-cancel-hook wiring'
fi

# ----------------------------------------------------------------------------
# 7. Example config documents the new key.
# ----------------------------------------------------------------------------
if grep -qE 'max_batch_spawns' examples/dev.yaml; then
    ok 'phase 186: examples/dev.yaml documents max_batch_spawns'
else
    fail 'phase 186: examples/dev.yaml missing the max_batch_spawns key'
fi

# ----------------------------------------------------------------------------
# 8. Build the touched packages.
# ----------------------------------------------------------------------------
if go build ./internal/runtime/dispatch/... ./internal/planner/... \
    ./internal/config/... ./internal/runtime/assemble/... 2>/dev/null; then
    ok 'phase 186: go build of the touched packages succeeds'
else
    fail 'phase 186: go build of the touched packages failed'
fi

# ----------------------------------------------------------------------------
# 9. The Batch dispatch test set passes under -race (dispatch table,
#    auto-group, breadth-cap rejection, FailFast disagreement, ordering
#    invariant, concurrent-reuse).
# ----------------------------------------------------------------------------
if go test -race -count=1 -run 'Batch' ./internal/runtime/dispatch/... 2>/dev/null; then
    ok 'phase 186: Batch dispatch tests pass under -race'
else
    fail 'phase 186: Batch dispatch test set failed under -race'
fi

# ----------------------------------------------------------------------------
# 10. The end-to-end integration suite passes under -race.
# ----------------------------------------------------------------------------
if go test -race -count=1 -run 'BatchExecutor' ./test/integration/... 2>/dev/null; then
    ok 'phase 186: BatchExecutor integration suite passes under -race'
else
    fail 'phase 186: BatchExecutor integration suite failed under -race'
fi

smoke_summary
