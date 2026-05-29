#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 107e — SpawnTask + AwaitTask dev-executor dispatch (background-task execution).
#
# Surface under test:
#   - cmd/harbor/cmd_dev_executor.go dispatches planner.SpawnTask + planner.AwaitTask
#     through the tasks.TaskRegistry instead of returning ErrDecisionShapeUnsupported.
#   - The per-task RunLoop driver drives KindBackground tasks (not just foreground),
#     so a spawned background sub-goal actually runs and reaches a terminal status.
#   - A parent run can spawn a background task (_spawn_task) and join it (_await_task)
#     within one dev run, bounded by planner.absolute_max_spawn_depth.
#
# 404/405/501 → SKIP convention (AGENTS.md §4.2) keeps this green on builds
# that predate the surface. The live assertions also SKIP without a provider
# key (the spawn-then-join elicitation needs a real model).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Static: the dev executor no longer hard-rejects SpawnTask / AwaitTask.
# ----------------------------------------------------------------------------
EXEC_SRC="cmd/harbor/cmd_dev_executor.go"
if [ -f "${EXEC_SRC}" ]; then
  if grep -qE 'SpawnTask \(background-task dispatcher lands post-V1\.1\)' "${EXEC_SRC}"; then
    fail "phase 107e: ${EXEC_SRC} still returns ErrDecisionShapeUnsupported for SpawnTask"
  else
    ok "phase 107e: ${EXEC_SRC} no longer hard-rejects SpawnTask"
  fi
else
  skip "phase 107e: ${EXEC_SRC} absent (pre-107e build)"
fi

# ----------------------------------------------------------------------------
# Live: spawn → background run → await join. Needs a provider key + a dev
# token. SKIP cleanly when either is missing.
#
# Real assertions land with the implementation (AC-15 / smoke steps 5-7):
#   - assert a task.spawned event for a KindBackground task during the parent run
#   - assert the spawned task reaches task.completed
#   - assert the parent trajectory carries a SpawnTask step + an AwaitTask step
#   - assert no ErrContextLeak in the server log
# ----------------------------------------------------------------------------
if [ -z "${HARBOR_DEV_TOKEN:-}" ]; then
  skip "phase 107e: no HARBOR_DEV_TOKEN — live spawn/await assertions skipped"
else
  skip "phase 107e: live spawn/await assertions — implement with AC-15 (scripted spawn-then-join run)"
fi

smoke_summary
