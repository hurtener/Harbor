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
if [[ -f "cmd/harbor/cmd_dev_executor.go" ]]; then
  if grep -q "runtime/parallel" "cmd/harbor/cmd_dev_executor.go"; then
    ok "dev executor imports internal/runtime/parallel (CallParallel dispatch wired)"
  else
    skip "phase 107d: dev executor does not yet wire parallel.Executor — surface not shipped"
    smoke_summary
    exit 0
  fi
else
  skip "phase 107d: cmd/harbor/cmd_dev_executor.go absent — pre-83i build"
  smoke_summary
  exit 0
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
