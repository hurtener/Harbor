#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 272 — durable cumulative task inference allocation.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
source scripts/smoke/common.sh
assert_file docs/plans/phase-272-task-inference-allocation.md "task allocation plan exists"
if go test -race -p 1 ./internal/llm/allocation ./internal/llm/drivers/bifrost ./internal/tasks/engine ./internal/tasks/protocol ./internal/runtime/serve ./internal/protocol \
  -run 'TestAllocation_|TestEngine_Allocation|TestPostureDispatch_AllocationFinality' -count=1; then
  ok "task allocation admission, inheritance, cumulative liability and provider bounds pass"
else
  fail "task allocation contract regression"
fi
smoke_summary
