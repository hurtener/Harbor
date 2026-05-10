#!/usr/bin/env bash
# Phase 15 smoke skeleton — SQLite StateStore driver.
#
# Phase 15 will ship internal/state/drivers/sqlite — the second leg of
# the Harbor persistence triad (RFC §9). Until that package lands, this
# smoke is skip-only (per scripts/smoke/_template.sh's pattern). The
# implementation PR replaces the skip with the real test invocation:
#
#   if go test -race -count=1 -timeout 90s ./internal/state/drivers/sqlite/... >/dev/null 2>&1; then
#       ok 'phase 15: internal/state/drivers/sqlite tests pass under -race (conformance + migrations + concurrent)'
#   else
#       fail 'phase 15: internal/state/drivers/sqlite tests failed (run `go test -race ./internal/state/drivers/sqlite/...` for detail)'
#   fi

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 15: SQLite StateStore driver — implementation pending; smoke skeleton awaits go-test wiring"
skip "phase 15: state/sqlite has no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
