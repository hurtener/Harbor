#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 10 smoke — Engine + workers + cycle detection.
#
# Phase 10 lands internal/runtime/engine: the typed, async, queue-
# backed graph executor. There is no HTTP / Protocol surface yet
# (Protocol exposure lands in Phase 60); correctness is verified by
# the Go test suite. The smoke runs the package's race-enabled tests
# directly and the cross-subsystem integration test at
# test/integration/.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Timeout aligned with phase-11.sh / phase-14.sh: the engine package's
# test set grows with every later phase that layers onto the engine
# (Phase 11 added retry/backoff sleeps; Phase 14 added subflow tests).
# A 60s timeout flaked on slower CI runners once the cumulative test
# time grew. Per AGENTS.md §17.6 ("Fix what the integration test finds
# — no matter where the bug lives"), this is bundled in Phase 11's PR.
if go test -race -count=1 -timeout 90s ./internal/runtime/engine/... >/dev/null 2>&1; then
    ok 'phase 10: internal/runtime/engine tests pass under -race'
else
    fail 'phase 10: internal/runtime/engine tests failed (run `go test -race ./internal/runtime/engine/...` for detail)'
fi

if go test -race -count=1 -timeout 90s -run '^TestE2E_Phase10_' ./test/integration/... >/dev/null 2>&1; then
    ok 'phase 10: cross-subsystem integration tests pass (TestE2E_Phase10_*)'
else
    fail 'phase 10: integration tests failed (run `go test -race -run TestE2E_Phase10_ ./test/integration/...`)'
fi

skip "phase 10: engine has no HTTP/Protocol surface yet (lands in Phase 60)"

smoke_summary
