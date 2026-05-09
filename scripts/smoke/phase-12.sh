#!/usr/bin/env bash
# Phase 12 smoke — Streaming + per-run capacity backpressure.
#
# Phase 12 layers StreamFrame + EmitChunk + per-run capacity waiters
# onto Phase 10's engine. The deadlock-prevention test
# (TestEmitChunk_CrossRun_NoDeadlock) is the gate per master plan.
# Timeout aligned with phase-10/11/14 (90s) so engine-package growth
# in later phases doesn't squeeze CI.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 90s ./internal/runtime/engine/... >/dev/null 2>&1; then
    ok 'phase 12: internal/runtime/engine tests pass under -race (includes streaming + capacity + deadlock-prevention gate)'
else
    fail 'phase 12: internal/runtime/engine tests failed (run `go test -race ./internal/runtime/engine/...` for detail)'
fi

if go test -race -count=1 -timeout 90s -run '^TestE2E_Phase12_' ./test/integration/... >/dev/null 2>&1; then
    ok 'phase 12: cross-subsystem integration tests pass (TestE2E_Phase12_*)'
else
    fail 'phase 12: integration tests failed (run `go test -race -run TestE2E_Phase12_ ./test/integration/...`)'
fi

skip "phase 12: streaming has no HTTP/Protocol surface yet (Phase 60+)"

smoke_summary
