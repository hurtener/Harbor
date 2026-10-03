#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 274 — trusted inclusive task monetary caps; synthetic transports only.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
source scripts/smoke/common.sh
assert_file docs/plans/phase-274-task-monetary-caps.md "trusted task monetary cap plan exists"
if go test -race -p 1 ./internal/llm/pricing ./internal/llm/allocation ./internal/llm/drivers/bifrost \
  ./internal/tasks/engine ./internal/tasks/drivers/durable ./internal/tasks/protocol \
  ./internal/runtime/assemble ./internal/runtime/serve ./internal/protocol \
  -run 'TestMonetary|TestEngine_Monetary' -count=1; then
  ok "trusted tariffs, exact admission, inclusive reservation and durable monetary liability pass"
else
  fail "task monetary cap contract regression"
fi
smoke_summary
