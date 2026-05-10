#!/usr/bin/env bash
# Phase 16 smoke skeleton — Postgres StateStore driver.
#
# Phase 16 will ship internal/state/drivers/postgres — the third leg of
# the Harbor persistence triad (RFC §9). Until that package lands, this
# smoke is skip-only (per scripts/smoke/_template.sh's pattern). The
# implementation PR replaces the skip with the real test invocation:
#
#   if go test -race -count=1 -timeout 120s ./internal/state/drivers/postgres/... >/dev/null 2>&1; then
#       ok 'phase 16: internal/state/drivers/postgres tests pass under -race (skip-clean without HARBOR_PG_DSN)'
#   else
#       fail 'phase 16: internal/state/drivers/postgres tests failed (run `go test -race ./internal/state/drivers/postgres/...` for detail)'
#   fi

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 16: Postgres StateStore driver — implementation pending; smoke skeleton awaits go-test wiring"
skip "phase 16: state/postgres has no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
