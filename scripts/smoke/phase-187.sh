#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 187 smoke — Task-management meta-tools (_task_status / _cancel_task),
# descendant scoping, model-expressible propagate_on_cancel:isolate, and the
# cascade-walk fix that makes isolate actually detach from an ancestor's
# cascade.
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.
#
# This is a unit-test-class smoke (source/build/test assertions, not a live
# server) — the meta-tools have no HTTP surface. It mirrors phase-186's
# static/unit pattern; the not-yet-built guard SKIPs the whole surface on
# pre-187 builds.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

DECISION="internal/planner/decision.go"
DISCOVERED="internal/planner/react/discovered_tools.go"
PROJECTOR="internal/planner/react/projector.go"
DISPATCH="internal/runtime/dispatch/dispatch.go"
ENGINE="internal/tasks/engine/engine.go"
GROUPSFILE="internal/tasks/engine/groups.go"

# ----------------------------------------------------------------------------
# 0. Not-yet-built guard: skip the whole surface if the two new sealed
#    Decision shapes aren't wired yet (the 404-equivalent for a static smoke).
# ----------------------------------------------------------------------------
if ! grep -qE 'func \(TaskStatusQuery\) isDecision' "${DECISION}"; then
    skip 'phase 187: TaskStatusQuery decision shape not yet wired — surface not built'
    smoke_summary
    exit 0
fi

# ----------------------------------------------------------------------------
# 1. Two new sealed Decision shapes exist (RFC §6.2: TaskStatusQuery /
#    CancelTask — NOT the tasks-package TaskStatus lifecycle enum).
# ----------------------------------------------------------------------------
if grep -qE 'func \(TaskStatusQuery\) isDecision' "${DECISION}" \
    && grep -qE 'func \(CancelTask\) isDecision' "${DECISION}"; then
    ok 'phase 187: TaskStatusQuery + CancelTask sealed Decision shapes present'
else
    fail 'phase 187: one of the new sealed Decision shapes is missing'
fi

# ----------------------------------------------------------------------------
# 2. SpawnSpec carries the model-expressible PropagateOnCancel brake.
# ----------------------------------------------------------------------------
if grep -qE 'PropagateOnCancel string' "${DECISION}"; then
    ok 'phase 187: SpawnSpec.PropagateOnCancel field present'
else
    fail 'phase 187: SpawnSpec.PropagateOnCancel field missing'
fi

# ----------------------------------------------------------------------------
# 3. Reserved declarations for the two meta-tools are present, and the
#    _spawn_task schema gained propagate_on_cancel.
# ----------------------------------------------------------------------------
if grep -qE 'Name: +TaskStatusToolName' "${DISCOVERED}" \
    && grep -qE 'Name: +CancelTaskToolName' "${DISCOVERED}" \
    && grep -qE 'jsonSchemaRawTaskStatus' "${DISCOVERED}" \
    && grep -qE 'jsonSchemaRawCancelTask' "${DISCOVERED}"; then
    ok 'phase 187: _task_status + _cancel_task reserved declarations present'
else
    fail 'phase 187: reserved declaration(s) for the meta-tools missing'
fi

if grep -qE 'propagate_on_cancel' "${DISCOVERED}"; then
    ok 'phase 187: _spawn_task schema carries propagate_on_cancel'
else
    fail 'phase 187: _spawn_task schema missing propagate_on_cancel'
fi

# ----------------------------------------------------------------------------
# 4. The projector standalone guard names both new tools (non-batchable
#    first grammar).
# ----------------------------------------------------------------------------
if grep -qE 'FinishToolName, AwaitTaskToolName, TaskStatusToolName, CancelTaskToolName' "${PROJECTOR}"; then
    ok 'phase 187: projector standalone guard rejects _task_status / _cancel_task co-occurrence'
else
    fail 'phase 187: projector standalone guard does not name the new meta-tools'
fi

# ----------------------------------------------------------------------------
# 5. Dispatch wires the two executor methods + the descendant-scope
#    sentinel.
# ----------------------------------------------------------------------------
if grep -qE 'case planner\.TaskStatusQuery:' "${DISPATCH}" \
    && grep -qE 'case planner\.CancelTask:' "${DISPATCH}" \
    && grep -qE 'ErrTaskNotOwnDescendant' "${DISPATCH}" \
    && grep -qE 'func \(e \*toolExecutor\) isOwnDescendant\(' "${DISPATCH}"; then
    ok 'phase 187: dispatch executor methods + descendant-scope sentinel present'
else
    fail 'phase 187: dispatch executor wiring for the meta-tools incomplete'
fi

# ----------------------------------------------------------------------------
# 6. The cascade-walk duplication is actually closed: the shared helper
#    name appears in BOTH engine.go and groups.go.
# ----------------------------------------------------------------------------
if grep -qE 'cascadeCancelDescendantsLocked' "${ENGINE}" \
    && grep -qE 'cascadeCancelDescendantsLocked' "${GROUPSFILE}"; then
    ok 'phase 187: shared cascade-walk helper used by both engine.go and groups.go'
else
    fail 'phase 187: cascade-walk duplication not closed (helper missing from one call site)'
fi

# ----------------------------------------------------------------------------
# 7. Build the touched packages.
# ----------------------------------------------------------------------------
if go build ./internal/planner/... ./internal/runtime/dispatch/... ./internal/tasks/... 2>/dev/null; then
    ok 'phase 187: go build of the touched packages succeeds'
else
    fail 'phase 187: go build of the touched packages failed'
fi

# ----------------------------------------------------------------------------
# 8. Projector translation + standalone-rejection coverage under -race.
# ----------------------------------------------------------------------------
if go test -race -count=1 \
    -run 'TaskStatus|CancelTask|TaskMgmt|PropagateOnCancel' \
    ./internal/planner/react/... 2>/dev/null; then
    ok 'phase 187: projector translation + standalone-rejection tests pass under -race'
else
    fail 'phase 187: projector meta-tool test set failed under -race'
fi

# ----------------------------------------------------------------------------
# 9. Descendant-scope rejection + concurrent-reuse under -race.
# ----------------------------------------------------------------------------
if go test -race -count=1 \
    -run 'TestExecutor_TaskStatus|TestExecutor_CancelTask|TestExecutor_TaskMgmt' \
    ./internal/runtime/dispatch/... 2>/dev/null; then
    ok 'phase 187: descendant-scope + concurrent-reuse dispatch tests pass under -race'
else
    fail 'phase 187: dispatch meta-tool test set failed under -race'
fi

# ----------------------------------------------------------------------------
# 10. Cascade-walk fix: isolate-vs-cascade and isolate-vs-operator-cancel
#     scenarios (engine + conformance across both drivers) pass under -race.
# ----------------------------------------------------------------------------
if go test -race -count=1 -run 'CancelHierarchy|Isolate' \
    ./internal/tasks/engine/... ./internal/tasks/conformancetest/... \
    ./internal/tasks/drivers/... 2>/dev/null; then
    ok 'phase 187: cascade-walk isolate scenarios pass under -race'
else
    fail 'phase 187: cascade-walk isolate scenario set failed under -race'
fi

smoke_summary
