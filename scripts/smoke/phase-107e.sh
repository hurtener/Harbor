#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 107e — SpawnTask + AwaitTask dev-executor dispatch (background-task execution).
#
# Surface under test:
#   - internal/runtime/dispatch (promoted from cmd/harbor by 110a / D-194)
#     dispatches planner.SpawnTask + planner.AwaitTask
#     through the tasks.TaskRegistry instead of returning ErrDecisionShapeUnsupported.
#   - The per-task RunLoop driver drives KindBackground tasks (not just foreground),
#     so a spawned background sub-goal actually runs and reaches a terminal status.
#   - A parent run can spawn a background task (_spawn_task) and join it (_await_task)
#     within one dev run, bounded by planner.absolute_max_spawn_depth.
#
# §4.3 conversion (2026-06-10, program follow-ups chore): the plan's
# original live LLM-elicited spawn-then-join (AC-15 / smoke steps 4-7)
# is covered by the static Go-test gate instead (the phase-110a smoke
# pattern): `internal/runtime/serve/spawn_await_test.go::
# TestSpawnThenAwait_BackgroundDrivenEndToEnd` runs the SAME semantics
# deterministically — real TaskRegistry + real per-task driver
# (driveBackground=true) + the promoted production executor, spawn →
# background run → await join, identity propagation, a failing-child
# sibling — under -race, with no provider key and no flaky elicitation
# prompt. The placeholder live section (which skipped even WITH a
# token) is deleted; the surface now shows OK > 0 under preflight
# (§4.2 rule 5).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Static: the dev executor no longer hard-rejects SpawnTask / AwaitTask.
# ----------------------------------------------------------------------------
# Phase 110a (D-194) promoted the executor out of cmd/harbor.
EXEC_SRC="internal/runtime/dispatch/dispatch.go"
if [ -f "${EXEC_SRC}" ]; then
  if grep -qE 'SpawnTask \(background-task dispatcher lands post-V1\.1\)' "${EXEC_SRC}"; then
    fail "phase 107e: ${EXEC_SRC} still returns ErrDecisionShapeUnsupported for SpawnTask"
  else
    ok "phase 107e: ${EXEC_SRC} no longer hard-rejects SpawnTask"
  fi
else
  skip "phase 107e: ${EXEC_SRC} absent (pre-110a build)"
fi

# ----------------------------------------------------------------------------
# AC-15 gate: spawn → background run → await join, end-to-end under -race.
# The driver-integrated E2E pair (cmd/harbor) proves the production
# wiring drives a spawned background task to completion and joins it;
# the dispatch slice covers the executor's pure spawn/await behaviour
# (depth cap, terminal polling, D-026 projection, failed child,
# concurrent reuse).
# ----------------------------------------------------------------------------
if [ ! -f "internal/runtime/serve/spawn_await_test.go" ]; then
  skip "phase 107e: internal/runtime/serve/spawn_await_test.go absent (pre-107e build)"
else
  if go test ./internal/runtime/serve/ \
      -run 'TestSpawnThenAwait_BackgroundDrivenEndToEnd|TestSpawnTask_RetainTurn_BlocksAndReturnsOutcome' \
      -race -count=1 -timeout 300s >/dev/null 2>&1; then
    ok "phase 107e: spawn→background-run→await-join E2E pair passes under -race (AC-15)"
  else
    fail "phase 107e: spawn/await driver E2Es failed (run: go test ./internal/runtime/serve/ -run 'TestSpawnThenAwait|TestSpawnTask_RetainTurn' -race)"
  fi

  if go test ./internal/runtime/dispatch/ \
      -run 'TestExecutor_Spawn|TestExecutor_Await' \
      -race -count=1 -timeout 300s >/dev/null 2>&1; then
    ok "phase 107e: executor spawn/await dispatch slice passes under -race"
  else
    fail "phase 107e: executor spawn/await dispatch slice failed (run: go test ./internal/runtime/dispatch/ -run 'TestExecutor_Spawn|TestExecutor_Await' -race)"
  fi
fi

smoke_summary
