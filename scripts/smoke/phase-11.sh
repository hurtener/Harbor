#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 11 smoke — Reliability shell.
#
# Phase 11 layers the reliability shell (NodePolicy / RunError /
# backoff math) onto Phase 10's engine and routes terminal errors
# through Phase 04's logger + Phase 05's bus via the wave-2 eventbus
# adapter. There is no HTTP/Protocol surface yet (Phase 60); the
# Go-package tests + cross-subsystem integration test carry the load.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 90s ./internal/runtime/engine/... >/dev/null 2>&1; then
    ok 'phase 11: internal/runtime/engine tests pass under -race (includes shell + backoff + reuse-with-policy)'
else
    fail 'phase 11: internal/runtime/engine tests failed (run `go test -race ./internal/runtime/engine/...` for detail)'
fi

if go test -race -count=1 -timeout 60s -run '^TestE2E_Phase11_' ./test/integration/... >/dev/null 2>&1; then
    ok 'phase 11: cross-subsystem integration test passes (TestE2E_Phase11_NodeFailure_BusEvent)'
else
    fail 'phase 11: integration test failed (run `go test -race -run TestE2E_Phase11_ ./test/integration/...`)'
fi

skip "phase 11: reliability shell has no HTTP/Protocol surface yet (Phase 60+)"

smoke_summary
