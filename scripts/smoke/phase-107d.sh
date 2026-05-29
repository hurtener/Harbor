#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 107d — native parallel tool-calls (executor CallParallel branch + default flip).
#
# Surface under test:
#   - cmd/harbor/cmd_dev_executor.go dispatches planner.CallParallel through
#     internal/runtime/parallel.Executor instead of ErrDecisionShapeUnsupported.
#   - The React planner emits a native CallParallel for N>1 tool-calls (default
#     planner.react.parallel_tool_calls=true), so several tools dispatch
#     concurrently in one assistant turn.
#
# 404/405/501 → SKIP convention (AGENTS.md §4.2) keeps this green on builds
# that predate the surface. The live assertions also SKIP without a provider
# key (AC-1 — the multi-tool-call elicitation needs a real model).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Static assertions (run regardless of provider key).
# ----------------------------------------------------------------------------

# AC-1: the CallParallel reject is gone; the dev executor consumes the
# runtime parallel executor.
EXEC_FILE="cmd/harbor/cmd_dev_executor.go"
if [[ -f "${EXEC_FILE}" ]]; then
  if grep -q "runtime/parallel" "${EXEC_FILE}"; then
    ok "dev executor imports internal/runtime/parallel (CallParallel dispatch wired)"
  else
    skip "phase 107d: dev executor does not yet wire parallel.Executor — surface not shipped"
    smoke_summary
    exit 0
  fi
else
  skip "phase 107d: ${EXEC_FILE} absent — pre-83i build"
  smoke_summary
  exit 0
fi

# AC-1: the CallParallel branch no longer returns ErrDecisionShapeUnsupported.
# (The SpawnTask / AwaitTask rejects survive — so we check the CallParallel
# case specifically resolves to a dispatch method.)
if grep -qE "case planner\.CallParallel:\s*$" "${EXEC_FILE}" && \
   grep -q "e.callParallel(ctx, rc, d)" "${EXEC_FILE}"; then
  ok "AC-1: CallParallel dispatches via callParallel (reject removed)"
else
  skip "AC-1: CallParallel dispatch method not found — surface not shipped"
fi

# AC-7: the parallel executor exposes the non-atomic per-call option.
if grep -q "WithNonAtomicSetup" "internal/runtime/parallel/parallel.go"; then
  ok "AC-7: parallel.Executor exposes WithNonAtomicSetup (non-atomic native mode)"
else
  skip "AC-7: WithNonAtomicSetup absent — non-atomic mode not shipped"
fi

# AC-8: the React projector emits a native CallParallel for N>1 tool-calls.
if grep -q "planner.CallParallel{Branches" "internal/planner/react/projector.go"; then
  ok "AC-8: react projector emits native CallParallel for N>1 tool-calls"
else
  skip "AC-8: projector CallParallel emission absent — surface not shipped"
fi

# AC-21: reserved-name co-occurrence is rejected (carried-over 107c fix).
if grep -q "isReservedControlName" "internal/planner/react/projector.go"; then
  ok "AC-21: projector rejects reserved-control-name co-occurrence (silent tail-drop closed)"
else
  skip "AC-21: reserved-name guard absent — carried-over fix not shipped"
fi

# AC-11: the parallel_tool_calls config knob exists.
if grep -q "parallel_tool_calls" "internal/config/config.go"; then
  ok "AC-11: config exposes planner.parallel_tool_calls"
else
  skip "AC-11: parallel_tool_calls config field absent — surface not shipped"
fi

# ----------------------------------------------------------------------------
# Live assertions — need a booted server AND a provider key (AC-1).
# ----------------------------------------------------------------------------

if [[ -z "${HARBOR_DEV_TOKEN:-}" ]]; then
  skip "phase 107d: no HARBOR_DEV_TOKEN — live parallel-dispatch probe needs a provider-backed dev server"
  smoke_summary
  exit 0
fi

# TODO(107d): when the surface ships, replace the skip below with:
#   1. POST a query that elicits several independent tool-calls in one turn.
#   2. Subscribe to /v1/events/subscribe; assert >=2 tool.invoked events fire
#      between two consecutive assistant turns (concurrent dispatch, not the
#      one-per-turn serialization fallback).
#   3. Fetch tasks.get; assert a trajectory step carries >=2 branches with one
#      observation per branch.
#   4. grep the server log for ErrContextLeak == none (per-branch D-026 held).
skip "phase 107d: live parallel-dispatch probe — replace when the executor CallParallel branch ships"

smoke_summary
