#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 18 smoke — ArtifactStore SQLite-blob + Postgres-blob drivers.
#
# Phase 18 ships internal/artifacts/drivers/{sqlite,postgres} — the
# durable artifact triad (RFC §6.10, §9). Both drivers inherit
# internal/artifacts/conformancetest.Run verbatim. SQLite tests run
# unconditionally (no external deps; modernc.org/sqlite is pure Go).
# Postgres tests skip cleanly without HARBOR_PG_DSN set (CI provides
# a postgres:16 service container so the suite actually exercises
# the driver there). The driver has no HTTP / Protocol surface yet
# (lands in Phase 60+).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 120s \
        ./internal/artifacts/drivers/sqlite/... \
        ./internal/artifacts/drivers/postgres/... >/dev/null 2>&1; then
    ok 'phase 18: internal/artifacts/drivers/{sqlite,postgres} tests pass under -race (skip-clean without HARBOR_PG_DSN)'
else
    fail 'phase 18: artifact-blob driver tests failed (run `go test -race ./internal/artifacts/drivers/{sqlite,postgres}/...` for detail)'
fi

skip "phase 18: artifact-blob has no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
