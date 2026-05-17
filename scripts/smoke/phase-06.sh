#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 06 smoke — bus replay + ring buffer + cursor.
#
# Phase 06 extends internal/events with the Replayer capability interface
# and an in-memory ring buffer on the inmem driver. The Protocol surface
# ships in Phase 60; until then the per-phase correctness gate is `go
# test -race` against the replay test suite. Per AGENTS.md §4.2.5 a
# SKIP that should be an OK is a bug, so the script asserts the new
# tests pass and only SKIPs the HTTP/Protocol surface that hasn't
# landed yet.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if [ -d "internal/events/drivers/inmem" ]; then
    if go test -race -count=1 -run "^TestReplay" ./internal/events/drivers/inmem/... >/tmp/phase-06-replay.log 2>&1; then
        ok "phase 06: TestReplay_* suite passes (ring buffer + cursor + filter)"
    else
        fail "phase 06: TestReplay_* suite failed"
        printf '--- go test output ---\n'
        cat /tmp/phase-06-replay.log
    fi
else
    skip "phase 06: internal/events/drivers/inmem absent (replay-equipped driver not yet implemented)"
fi

if [ -d "test/integration" ]; then
    if go test -race -count=1 -run "^TestE2E_Phase06_" ./test/integration/... >/tmp/phase-06-integration.log 2>&1; then
        ok "phase 06: TestE2E_Phase06_* integration tests pass (Logger.Error → bus → Replay end-to-end)"
    else
        fail "phase 06: TestE2E_Phase06_* failed"
        printf '--- go test output ---\n'
        cat /tmp/phase-06-integration.log
    fi
fi

skip "phase 06: events replay has no HTTP/Protocol surface yet (Phase 60+)"

smoke_summary
