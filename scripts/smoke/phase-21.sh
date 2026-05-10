#!/usr/bin/env bash
# Phase 21 smoke skeleton — TaskGroup + retain-turn + patches.
#
# Phase 21 will extend internal/tasks with group governance, the
# retain-turn waiter mechanism, and the ApplyPatch /
# AcknowledgeBackground surface (RFC §6.8). Until the implementation
# lands, this smoke is skip-only. Replaces with:
#
#   if go test -race -count=1 -timeout 90s ./internal/tasks/... >/dev/null 2>&1; then
#       ok 'phase 21: internal/tasks tests pass under -race (groups + retain-turn + patches)'
#   else
#       fail 'phase 21: internal/tasks tests failed'
#   fi

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 21: TaskGroup + retain-turn + patches — implementation pending; smoke skeleton awaits go-test wiring"
skip "phase 21: task groups have no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
