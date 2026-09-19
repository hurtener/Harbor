#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 269 — explicitly enabled terminal execution-context retention.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
source scripts/smoke/common.sh
assert_file docs/plans/phase-269-retained-session-context.md "retained context plan exists"
assert_grep_present '^## D-464 ' docs/decisions.md "retained window decision exists"
if go test -race ./internal/runtime/runctx ./internal/runtime/assemble ./sdk/assemble \
    -run 'TestRetainedContext_|TestRunOnce_RetainedContext' -count=1; then
    ok "retained evidence, restore, erasure and embedded request regressions pass"
else
    fail "retained execution context regression failed"
fi
smoke_summary
