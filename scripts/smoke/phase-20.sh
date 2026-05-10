#!/usr/bin/env bash
# Phase 20 smoke skeleton — TaskRegistry interface + InProcess driver.
#
# Phase 20 will ship internal/tasks: the unified TaskID namespace
# (foreground + background), TaskRegistry interface, InProcess
# driver, lifecycle state machine, idempotency, cancellation
# propagation (RFC §6.8). Until the implementation lands, this smoke
# is skip-only. The implementation PR replaces the skip with the
# real test invocation:
#
#   if go test -race -count=1 -timeout 90s ./internal/tasks/... >/dev/null 2>&1; then
#       ok 'phase 20: internal/tasks tests pass under -race'
#   else
#       fail 'phase 20: internal/tasks tests failed'
#   fi

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 20: TaskRegistry — implementation pending; smoke skeleton awaits go-test wiring"
skip "phase 20: tasks have no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
