#!/usr/bin/env bash
# Phase 53a smoke — Agent Registry (registration identity + three-ID model).
#
# Phase 53a lands internal/runtime/registry as a typed subsystem over
# Phase 07's StateStore + Phase 05's EventBus. There is no HTTP /
# Protocol surface yet (the Console Agents page + its feeding Protocol
# surface land in the 54+ / 72-75 waves); correctness is verified by
# the Go test suite. The smoke runs the package's race-enabled tests,
# the cross-subsystem integration test, and `go vet` directly.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 120s ./internal/runtime/registry/... >/dev/null 2>&1; then
    ok 'phase 53a: internal/runtime/registry tests pass under -race'
else
    fail 'phase 53a: internal/runtime/registry tests failed (run `go test -race ./internal/runtime/registry/...` for detail)'
fi

if go test -race -count=1 -timeout 120s -run '^TestE2E_Phase53a_' ./test/integration/... >/dev/null 2>&1; then
    ok 'phase 53a: cross-subsystem integration tests pass (TestE2E_Phase53a_*)'
else
    fail 'phase 53a: integration tests failed (run `go test -race -run TestE2E_Phase53a_ ./test/integration/...`)'
fi

if go vet ./internal/runtime/registry/... >/dev/null 2>&1; then
    ok 'phase 53a: go vet clean on internal/runtime/registry'
else
    fail 'phase 53a: go vet reported issues on internal/runtime/registry'
fi

skip "phase 53a: Agent Registry has no HTTP/Protocol surface yet (Console Agents page + Protocol surface land in the 54+ / 72-75 waves)"

smoke_summary
