#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 47 smoke — Parallel-call executor + ReAct CallParallel /
# SpawnTask / AwaitTask emission upgrade (RFC §6.2; master plan
# Phase 47 detail block; D-056).
#
# Phase 47 lands the runtime parallel executor at
# `internal/runtime/parallel/` AND closes three primitive-with-consumer
# gaps in ReAct (per §13's "primitive must ship with consumer in same
# wave" rule):
#
#   1. CallParallel runtime executor — consumer: ReAct pass-through
#      (reduceToSingleAction deletion is the load-bearing two-parallel-
#      implementations cleanup).
#   2. SpawnTask emission — consumer: ReAct's _spawn_task reserved
#      tool translation + runtime task spawn / WatchGroup wake.
#   3. AwaitTask emission — consumer: ReAct's _await_task reserved
#      tool translation.
#
# This is a code-only phase; no protocol surface lands until
# Phase 60+. Static guards + per-package tests + integration test
# under -race.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. Per-package tests under -race. Covers the runtime parallel
#    executor + the upgraded react package emission paths +
#    D-025 reuse stress. Bumped to 300s to absorb cold-build CPU
#    pressure when other preflight smokes share the build cache.
if go test -race -count=1 -timeout 300s ./internal/runtime/parallel/... ./internal/planner/react/... >/dev/null 2>&1; then
    ok 'phase 47: internal/runtime/parallel + internal/planner/react tests pass under -race'
else
    fail 'phase 47: runtime/parallel or planner/react tests failed (run `go test -race ./internal/runtime/parallel/... ./internal/planner/react/...` for detail)'
fi

# 2. Integration test — the spawn → group → wake → planner re-entry
#    round-trip plus the real-catalog CallParallel end-to-end.
if go test -race -count=1 -timeout 300s -run TestE2E_Phase47 ./test/integration/... >/dev/null 2>&1; then
    ok 'phase 47: TestE2E_Phase47_* integration tests pass under -race (spawn→wake→re-entry; CallParallel; atomic-setup; cap)'
else
    fail 'phase 47: TestE2E_Phase47_* integration tests failed'
fi

# 3. Reserved-name constants pinned. The three Phase 47 emission shapes
#    rely on the reserved names (`_finish` is Phase 45's; `_spawn_task`
#    / `_await_task` are Phase 47's). A silent rename breaks the
#    prompt schema → mapDecision contract.
REACT_FILE="internal/planner/react/react.go"
if grep -q '^const FinishToolName = "_finish"' "${REACT_FILE}" \
    && grep -q '^const SpawnTaskToolName = "_spawn_task"' "${REACT_FILE}" \
    && grep -q '^const AwaitTaskToolName = "_await_task"' "${REACT_FILE}"; then
    ok 'phase 47: ReAct reserved tool names pinned (_finish, _spawn_task, _await_task — D-056)'
else
    fail 'phase 47: ReAct reserved tool name constants drift in internal/planner/react/react.go (D-056)'
fi

# 4. reduceToSingleAction MUST be absent — the §13 two-parallel-
#    implementations cleanup. The Phase 47 PR deletes the override;
#    a re-introduction is the canonical drift signal.
if grep -rIn --include='*.go' 'reduceToSingleAction' internal/planner/react/ 2>/dev/null | grep -q .; then
    fail 'phase 47: reduceToSingleAction is present in internal/planner/react/ — Phase 47 D-056 DELETES this override (§13 two-parallel-implementations cleanup)'
else
    ok 'phase 47: reduceToSingleAction ABSENT from internal/planner/react/ (Phase 47 deletion confirmed — §13)'
fi

# 5. JoinKind constants pinned. JoinAll / JoinFirstSuccess / JoinN
#    are the three V1 join shapes; JoinKeyed is a documented future
#    surface. A silent rename breaks every dependent (the runtime
#    executor's dispatch switch).
DECISION_FILE="internal/planner/decision.go"
for kind in 'JoinAll JoinKind = "all"' 'JoinFirstSuccess JoinKind = "first_success"' 'JoinN JoinKind = "n"'; do
    if grep -q "${kind}" "${DECISION_FILE}"; then
        ok "phase 47: JoinKind constant pinned: ${kind}"
    else
        fail "phase 47: JoinKind constant missing or drift: ${kind} (D-056)"
    fi
done

# 6. AbsoluteMaxParallel = 50 system cap pinned. RFC §6.2 settled
#    value; the runtime executor enforces.
ERRORS_FILE="internal/planner/errors.go"
if grep -q '^const AbsoluteMaxParallel = 50' "${ERRORS_FILE}"; then
    ok 'phase 47: AbsoluteMaxParallel = 50 system cap pinned (RFC §6.2 / D-056)'
else
    fail 'phase 47: AbsoluteMaxParallel constant drift in internal/planner/errors.go (RFC §6.2 settled value is 50)'
fi

# 7. ErrParallelCapExceeded + ErrParallelInvalidJoin +
#    ErrParallelBranchInvalidArgs + ErrParallelPauseUnsupported
#    sentinels pinned. The executor's atomic-setup-validation
#    contract surfaces through these.
for sentinel in 'ErrParallelCapExceeded' 'ErrParallelInvalidJoin' 'ErrParallelBranchInvalidArgs' 'ErrParallelPauseUnsupported'; do
    if grep -q "${sentinel}" "${ERRORS_FILE}"; then
        ok "phase 47: ${sentinel} sentinel pinned"
    else
        fail "phase 47: ${sentinel} sentinel missing from internal/planner/errors.go (D-056)"
    fi
done

# 8. §13 import-graph guard — internal/planner/react/ must NOT import
#    internal/runtime/parallel/ (the planner subtree must NOT import
#    internal/runtime/... per Phase 42's lint).
if grep -rIn --include='*.go' 'github.com/hurtener/Harbor/internal/runtime' internal/planner/react/ 2>/dev/null | grep -q .; then
    fail 'phase 47: internal/planner/react/ imports internal/runtime/... — violates Phase 42 import-graph contract (§13)'
else
    ok 'phase 47: internal/planner/react/ does not import internal/runtime/... (§13 import-graph contract preserved)'
fi

# 9. The runtime parallel executor consumes the planner package (the
#    forward direction is fine; the §13 contract bans the reverse).
if grep -rIn --include='*.go' '"github.com/hurtener/Harbor/internal/planner"' internal/runtime/parallel/ 2>/dev/null | grep -q .; then
    ok 'phase 47: internal/runtime/parallel/ imports internal/planner (forward dependency — executor is the consumer; §13 one-way contract preserved)'
else
    fail 'phase 47: internal/runtime/parallel/ does NOT import internal/planner — the executor should be a typed consumer of planner.CallParallel (D-056)'
fi

# 10. Reserved-tool constants — Phase 47's D-056 pins the three names
#     in `react.go` source so the projector can intercept them when the
#     LLM emits them. Phase 107c step 10/11 audit removed the names
#     from the PROMPT BODY (mentioning `_finish` even as "RETIRED"
#     primes the legacy JSON-envelope shape on RLHF-trained models);
#     `_spawn_task` and `_await_task` ride into `req.Tools` as
#     synthetic native declarations instead. This smoke continues to
#     pin the source-file constants — the projector + `mapDecision`
#     reserved-name interception still depend on them.
for name in '_finish' '_spawn_task' '_await_task'; do
    if grep -q "${name}" "${REACT_FILE}"; then
        ok "phase 47: react.go pins reserved tool constant ${name}"
    else
        fail "phase 47: react.go missing reserved tool constant ${name} (D-056 — projector requires it)"
    fi
done

skip "phase 47: parallel executor + ReAct emission upgrade is code-only; protocol surface lands in Phase 60+"

smoke_summary
