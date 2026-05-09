#!/usr/bin/env bash
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

if go test -race -count=1 -timeout 60s ./internal/runtime/engine/... >/dev/null 2>&1; then
    ok 'phase 10: internal/runtime/engine tests pass under -race'
else
    fail 'phase 10: internal/runtime/engine tests failed (run `go test -race ./internal/runtime/engine/...` for detail)'
fi

if go test -race -count=1 -timeout 60s -run '^TestE2E_Phase10_' ./test/integration/... >/dev/null 2>&1; then
    ok 'phase 10: cross-subsystem integration tests pass (TestE2E_Phase10_*)'
else
    fail 'phase 10: integration tests failed (run `go test -race -run TestE2E_Phase10_ ./test/integration/...`)'
fi

skip "phase 10: engine has no HTTP/Protocol surface yet (lands in Phase 60)"

smoke_summary
