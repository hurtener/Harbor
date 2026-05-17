#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 20 smoke — TaskRegistry interface + InProcess driver.
#
# Phase 20 ships internal/tasks: the unified TaskID namespace
# (foreground + background), TaskRegistry interface, InProcess
# driver, lifecycle FSM (Pending → Running → Complete with Paused →
# Running and terminal Failed/Cancelled), idempotency, cancellation
# propagation (RFC §6.8). The smoke runs the package test suite
# (conformance run + InProcess driver tests + registry-surface unit
# tests) under -race. There is no HTTP / Protocol surface yet
# (lands in Phase 60+).
#
# SpawnTool's execution body is a no-op stub at Phase 20 (the task
# persists at StatusPending until the Phase 26 dispatcher wires the
# real execution). Documented inline in
# `internal/tasks/drivers/inprocess/inprocess.go`.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 90s ./internal/tasks/... >/dev/null 2>&1; then
    ok 'phase 20: internal/tasks tests pass under -race (conformance + InProcess driver + registry surface)'
else
    fail 'phase 20: internal/tasks tests failed (run `go test -race ./internal/tasks/...` for detail)'
fi

skip "phase 20: tasks have no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
