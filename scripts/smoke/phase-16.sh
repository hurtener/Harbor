#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 16 smoke — Postgres StateStore driver.
#
# Runs the driver's Go tests under -race. When HARBOR_PG_DSN is unset
# (most local-dev runs), every Postgres-touching subtest t.Skips
# cleanly and `go test` exits 0; the smoke reports OK because the
# package compiled and all tests resolved (skipped tests count as
# passing). CI sets HARBOR_PG_DSN against a postgres:16 service
# container so the suite actually exercises the driver there.
#
# The driver has no HTTP / Protocol surface yet; that lands in Phase
# 60+. The second skip line documents that.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 120s ./internal/state/drivers/postgres/... >/dev/null 2>&1; then
    ok 'phase 16: internal/state/drivers/postgres tests pass under -race (skip-clean without HARBOR_PG_DSN)'
else
    fail 'phase 16: internal/state/drivers/postgres tests failed (run `go test -race ./internal/state/drivers/postgres/...` for detail)'
fi

skip "phase 16: state/postgres has no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
