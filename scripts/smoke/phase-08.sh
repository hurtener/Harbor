#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 08 smoke — SessionRegistry + lifecycle + GC.
#
# Phase 08 lands internal/sessions as a typed wrapper over Phase 07's
# StateStore. There is no HTTP / Protocol surface yet (sessions Protocol
# methods land in Phase 60); correctness is verified by the Go test
# suite. The smoke runs the package's race-enabled tests directly and
# the cross-subsystem integration test at test/integration/.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 60s ./internal/sessions/... >/dev/null 2>&1; then
    ok 'phase 08: internal/sessions tests pass under -race'
else
    fail 'phase 08: internal/sessions tests failed (run `go test -race ./internal/sessions/...` for detail)'
fi

if go test -race -count=1 -timeout 60s -run '^TestE2E_Phase08_' ./test/integration/... >/dev/null 2>&1; then
    ok 'phase 08: cross-subsystem integration tests pass (TestE2E_Phase08_*)'
else
    fail 'phase 08: integration tests failed (run `go test -race -run TestE2E_Phase08_ ./test/integration/...`)'
fi

skip "phase 08: sessions has no HTTP/Protocol surface yet (lands in Phase 60)"

smoke_summary
