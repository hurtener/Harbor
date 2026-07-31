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
# real execution). Documented inline in the shared task engine
# `internal/tasks/engine/engine.go` (the inprocess + durable drivers
# wrap it; extracted in Phase 87).

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

# --- The spawn idempotency index key is the FULL identity triple. ---
#
# The aggregate `go test` above collapses to ONE ok line, so a dropped
# subtest and a passing one look identical from here. These two static
# guards fail loudly on the specific regression: dedup scoped by the
# session alone let a colliding (session, key) pair reach across a
# tenant boundary as a denial + existence oracle. The key holds the
# boundary structurally; the entropy of a session id is not the
# boundary (CLAUDE.md §6 rule 2).
IDEM_KEY_DECL="$(sed -n '/^type idempotencyKey struct {/,/^}/p' internal/tasks/engine/engine.go)"
if grep -q 'TenantID' <<<"${IDEM_KEY_DECL}" && grep -q 'UserID' <<<"${IDEM_KEY_DECL}"; then
    ok 'phase 20: the spawn idempotency index key carries the full identity triple (tenant + user + session)'
else
    fail 'phase 20: idempotencyKey dropped an isolation component — dedup must never reach across a tenant/user boundary'
fi

# The conformance suite is the driver-agnostic home for the invariant, so
# a second driver inherits it. Assert the colliding-key subtests are
# actually present rather than trusting the aggregate run above.
if grep -q 'Spawn_DifferentTenantsCanReuseKey' internal/tasks/conformancetest/conformancetest.go &&
    grep -q 'Spawn_DifferentUsersCanReuseKey' internal/tasks/conformancetest/conformancetest.go; then
    ok 'phase 20: the conformance suite pins the cross-tenant + cross-user colliding-key case for every driver'
else
    fail 'phase 20: the conformance suite lost its colliding (session, key) isolation subtests'
fi

skip "phase 20: tasks have no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
