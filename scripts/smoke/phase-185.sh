#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 185 smoke — Batch decision + AC-21 supersession (projector).
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.
#
# This is a unit-test-class smoke (source/build assertions, not a live
# server) — the Batch decision + projector partitioning have no HTTP
# surface. It mirrors phase-184's static-only pattern.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

DECISION="internal/planner/decision.go"
PROJECTOR="internal/planner/react/projector.go"
DISCOVERED="internal/planner/react/discovered_tools.go"

# ----------------------------------------------------------------------------
# 0. Not-yet-built guard: skip the whole surface if Batch isn't defined yet
#    (the 404-equivalent for a static smoke).
# ----------------------------------------------------------------------------
if ! grep -qE 'type Batch struct' "${DECISION}"; then
    skip 'phase 185: planner.Batch not yet defined — surface not built'
    smoke_summary
    exit 0
fi

# ----------------------------------------------------------------------------
# 1. Batch is a sealed Decision shape.
# ----------------------------------------------------------------------------
if grep -qE 'type Batch struct' "${DECISION}" \
    && grep -qE 'func \(Batch\) isDecision\(\)' "${DECISION}"; then
    ok 'phase 185: planner.Batch declared as a sealed Decision shape'
else
    fail 'phase 185: planner.Batch missing type or isDecision() marker'
fi

# ----------------------------------------------------------------------------
# 2. NewBatch validating constructor exists.
# ----------------------------------------------------------------------------
if grep -qE 'func NewBatch\(' "${DECISION}"; then
    ok 'phase 185: NewBatch validating constructor exists'
else
    fail 'phase 185: NewBatch constructor missing'
fi

# ----------------------------------------------------------------------------
# 3. SpawnTask gains the additive CallID field (mirrors CallTool.CallID).
# ----------------------------------------------------------------------------
if grep -qE 'CallID string' "${DECISION}" \
    && awk '/type SpawnTask struct/{f=1} f&&/CallID string/{print;exit}' "${DECISION}" | grep -q CallID; then
    ok 'phase 185: SpawnTask.CallID field present'
else
    fail 'phase 185: SpawnTask.CallID field missing'
fi

# ----------------------------------------------------------------------------
# 4. AC-21′: the standalone guard's reserved-name predicate is exactly
#    {FinishToolName, AwaitTaskToolName} — _spawn_task is NO LONGER standalone.
# ----------------------------------------------------------------------------
if grep -qE 'func isStandaloneControlName' "${PROJECTOR}"; then
    GUARD="$(awk '/func isStandaloneControlName/{f=1} f{print} f&&/^}/{exit}' "${PROJECTOR}")"
    if echo "${GUARD}" | grep -qE 'case FinishToolName, AwaitTaskToolName' \
        && ! echo "${GUARD}" | grep -q 'SpawnTaskToolName'; then
        ok 'phase 185: AC-21′ standalone guard is {FinishToolName, AwaitTaskToolName}; _spawn_task excluded'
    else
        fail 'phase 185: standalone guard case list is not exactly {FinishToolName, AwaitTaskToolName}'
    fi
else
    fail 'phase 185: isStandaloneControlName guard not found'
fi

# ----------------------------------------------------------------------------
# 5. The projector has a Batch partition path.
# ----------------------------------------------------------------------------
if grep -qE 'func projectBatch\(' "${PROJECTOR}" \
    && grep -qE 'responseHasSpawn\(' "${PROJECTOR}"; then
    ok 'phase 185: projector Batch partition path (projectBatch + responseHasSpawn) present'
else
    fail 'phase 185: projector Batch partition path missing'
fi

# ----------------------------------------------------------------------------
# 6. The _spawn_task prompt teaches the batching contract (closes the
#    prompt-vs-validator disagreement): the pre-fix phrase is gone; the new
#    text mentions co-occurrence.
# ----------------------------------------------------------------------------
if grep -q 'Use to launch parallel work' "${DISCOVERED}"; then
    fail 'phase 185: _spawn_task description still carries the pre-fix "Use to launch parallel work" phrasing'
elif grep -qiE 'alongside|co-occur|same response' "${DISCOVERED}"; then
    ok 'phase 185: _spawn_task description teaches the batching (co-occurrence) contract'
else
    fail 'phase 185: _spawn_task description does not teach co-occurrence'
fi

# ----------------------------------------------------------------------------
# 7. DecisionInvocationCount gains a Batch case (Tools counted, Spawns zero).
# ----------------------------------------------------------------------------
if grep -qE 'case Batch:' internal/planner/trajectory.go \
    && grep -qE 'case \*Batch:' internal/planner/trajectory.go; then
    ok 'phase 185: DecisionInvocationCount handles Batch and *Batch'
else
    fail 'phase 185: DecisionInvocationCount Batch case missing'
fi

# ----------------------------------------------------------------------------
# 8. Build the planner surface (the sealed sum still closes).
# ----------------------------------------------------------------------------
if go build ./internal/planner/... 2>/dev/null; then
    ok 'phase 185: go build ./internal/planner/... succeeds'
else
    fail 'phase 185: go build ./internal/planner/... failed'
fi

# ----------------------------------------------------------------------------
# 9. The focused test set passes under -race.
# ----------------------------------------------------------------------------
if go test -race -count=1 \
    -run 'Batch|AC21|ReservedDecl|ProjectResponse|ProjectBatch|DecisionInvocationCount|Serialize_Batch' \
    ./internal/planner/... ./internal/planner/react/... ./internal/planner/conformance/... 2>/dev/null; then
    ok 'phase 185: focused Batch/AC-21′/DecisionInvocationCount tests pass under -race'
else
    fail 'phase 185: focused Batch test set failed under -race'
fi

# ----------------------------------------------------------------------------
# 10. Batch registered in the conformance pack's sealed-sum assertion.
# ----------------------------------------------------------------------------
if grep -qE 'var _ planner\.Decision = planner\.Batch\{\}' internal/planner/conformance/conformance.go; then
    ok 'phase 185: Batch registered in the conformance Sealed_DecisionSum assertion'
else
    fail 'phase 185: Batch not registered in the conformance sealed-sum assertion'
fi

smoke_summary
