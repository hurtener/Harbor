#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 15 smoke — SQLite StateStore driver.
#
# Phase 15 lands internal/state/drivers/sqlite — the second leg of the
# Harbor persistence triad (RFC §9 + §6.11), backed by modernc.org/sqlite
# (CGo-free per AGENTS.md §5 + D-013). The driver passes the Phase 07
# conformance suite verbatim; the supplemental concurrent and migration
# tests live in the same package. There is no HTTP / Protocol surface
# yet (lands in Phase 60+); correctness is verified by the Go test suite.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 90s ./internal/state/drivers/sqlite/... >/dev/null 2>&1; then
    ok 'phase 15: internal/state/drivers/sqlite tests pass under -race (conformance + migrations + concurrent)'
else
    fail 'phase 15: internal/state/drivers/sqlite tests failed (run `go test -race ./internal/state/drivers/sqlite/...` for detail)'
fi

skip "phase 15: state/sqlite has no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
