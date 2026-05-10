#!/usr/bin/env bash
# Phase 13 smoke — Cancellation + per-run fetch dispatcher.
#
# Phase 13 fills Phase 10's Cancel + FetchByRun stubs. Cancel performs
# the 4-step propagation (flag → drain → cancel workers → release
# capacity + drain subqueue) and emits runtime.run_cancelled on the
# bus. FetchByRun reads from the dispatcher's per-run subqueue with
# the single-fetcher contract (brief 01 §5). The per-run subqueue
# write becomes blocking once a FetchByRun consumer subscribes,
# preserving Phase 12's cross-run no-deadlock guarantee for Fetch-
# only workloads. Engine-Cancel mirroring extends Phase 14's subflow
# so parent.Cancel(parentRunID) propagates to the child engine.
# Timeout aligned with phase-10/11/12/14 (90s) so engine-package
# growth in later phases doesn't squeeze CI.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 90s ./internal/runtime/engine/... >/dev/null 2>&1; then
    ok 'phase 13: internal/runtime/engine tests pass under -race (includes Cancel + FetchByRun + subflow Cancel mirroring)'
else
    fail 'phase 13: internal/runtime/engine tests failed (run `go test -race ./internal/runtime/engine/...` for detail)'
fi

if go test -race -count=1 -timeout 90s -run '^TestE2E_Phase13_' ./test/integration/... >/dev/null 2>&1; then
    ok 'phase 13: cross-subsystem integration tests pass (TestE2E_Phase13_*)'
else
    fail 'phase 13: integration tests failed (run `go test -race -run TestE2E_Phase13_ ./test/integration/...`)'
fi

skip "phase 13: cancellation has no HTTP/Protocol surface yet (Phase 60+)"

smoke_summary
