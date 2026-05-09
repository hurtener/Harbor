#!/usr/bin/env bash
# Phase 14 smoke — Routers + concurrency + Subflow.
#
# Phase 14 lands internal/runtime/routers (PredicateRouter,
# UnionRouter, RoutePolicy), internal/runtime/concurrency
# (MapConcurrent, JoinK), and internal/runtime/engine/subflow.go
# (CallSubflow with ctx-based cancel mirroring). There is no HTTP /
# Protocol surface yet (Phase 60); correctness is verified by the Go
# test suite.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 60s ./internal/runtime/routers/... ./internal/runtime/concurrency/... ./internal/runtime/engine/... >/dev/null 2>&1; then
    ok 'phase 14: routers + concurrency + engine (with Subflow) tests pass under -race'
else
    fail 'phase 14: package tests failed (run `go test -race ./internal/runtime/{routers,concurrency,engine}/...`)'
fi

if go test -race -count=1 -timeout 90s -run '^TestE2E_Phase14_' ./test/integration/... >/dev/null 2>&1; then
    ok 'phase 14: cross-subsystem integration tests pass (TestE2E_Phase14_*)'
else
    fail 'phase 14: integration tests failed (run `go test -race -run TestE2E_Phase14_ ./test/integration/...`)'
fi

skip "phase 14: routers + concurrency + Subflow have no HTTP/Protocol surface yet (lands in Phase 60)"

smoke_summary
