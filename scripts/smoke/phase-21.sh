#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 21 smoke — TaskGroup + retain-turn + patches + WatchGroup.
#
# Phase 21 extends internal/tasks with group governance (Open → Sealed
# → Completed | Cancelled lifecycle FSM), the retain-turn waiter that
# blocks the foreground turn until a group of background tasks
# resolves, the ApplyPatch / AcknowledgeBackground surface, and —
# centrally — the `WatchGroup` wake mechanism + `GroupCompletion`
# typed payload (RFC §6.8, brief 05, D-025 + D-027 + D-030).
#
# The smoke runs the package test suite (Phase 20 + Phase 21
# conformance subtests + InProcess driver tests + registry-surface
# unit tests) under -race. There is no HTTP / Protocol surface yet
# (lands in Phase 60+).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 90s ./internal/tasks/... >/dev/null 2>&1; then
    ok 'phase 21: internal/tasks tests pass under -race (Phase 20 + Phase 21 conformance + group/patch/WatchGroup driver tests)'
else
    fail 'phase 21: internal/tasks tests failed (run `go test -race ./internal/tasks/...` for detail)'
fi

skip "phase 21: task groups have no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
