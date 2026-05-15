#!/usr/bin/env bash
# Phase 57 smoke — durable event log driver (StateStore-backed).
#
# Phase 57 ships internal/events/drivers/durable: an EventBus +
# Replayer driver that persists every event through the StateStore so
# replay-from-cursor is exact and gap-free across Runtime restarts.
# The Protocol / HTTP surface for the event stream is Phase 60; until
# then the per-phase correctness gate is `go test -race` against the
# durable driver suite + the cross-StateStore-driver integration test.
# Per AGENTS.md §4.2.5 a SKIP that should be an OK is a bug, so the
# script asserts the new tests pass and only SKIPs the surface that
# has not landed yet.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if [ -d "internal/events/drivers/durable" ]; then
    if go test -race -count=1 -run "^TestDurable" ./internal/events/drivers/durable/... >/tmp/phase-57-durable.log 2>&1; then
        ok "phase 57: TestDurable_* suite passes (persist + replay + loud degradation)"
    else
        fail "phase 57: TestDurable_* suite failed"
        printf -- '--- go test output ---\n'
        cat /tmp/phase-57-durable.log
    fi

    if go test -race -count=1 -run "^TestConcurrentReuse_DurableBus$" ./internal/events/drivers/durable/... >/tmp/phase-57-concurrent.log 2>&1; then
        ok "phase 57: TestConcurrentReuse_DurableBus passes (D-025 N=120 concurrent publishers)"
    else
        fail "phase 57: TestConcurrentReuse_DurableBus failed"
        printf -- '--- go test output ---\n'
        cat /tmp/phase-57-concurrent.log
    fi
else
    skip "phase 57: internal/events/drivers/durable absent (durable event log not yet implemented)"
fi

if [ -d "test/integration" ]; then
    if go test -race -count=1 -run "^TestE2E_Phase57_" ./test/integration/... >/tmp/phase-57-integration.log 2>&1; then
        ok "phase 57: TestE2E_Phase57_* integration tests pass (durable replay across all StateStore drivers)"
    else
        fail "phase 57: TestE2E_Phase57_* failed"
        printf -- '--- go test output ---\n'
        cat /tmp/phase-57-integration.log
    fi
fi

skip "phase 57: durable event log has no HTTP/Protocol surface yet (Phase 60+)"

smoke_summary
