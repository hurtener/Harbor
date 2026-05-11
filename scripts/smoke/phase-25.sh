#!/usr/bin/env bash
# Phase 25 smoke — SQLite + Postgres MemoryStore drivers.
#
# Phase 25 lands `internal/memory/drivers/sqlite` + `internal/memory/drivers/postgres`,
# the persistent legs of the memory persistence triad (in-memory
# floor from Phase 23 + SQLite + Postgres). Both drivers run the
# shared conformance suite (`internal/memory/conformancetest`), pass
# the `Snapshot/Restore` byte-stable round-trip, and ship under
# `-race`.
#
# Skip-clean without HARBOR_PG_DSN: the postgres tests `t.Skip`
# cleanly when the env var is unset; SQLite always runs (modernc.org
# is CGo-free + bundles the engine, no service container needed).
# CI's `memory-postgres` job sets HARBOR_PG_DSN against the
# postgres:16 service container so the suite actually exercises the
# driver there.
#
# Memory has no HTTP / Protocol surface yet (lands in Phase 60+);
# correctness is verified by `go test -race ./internal/memory/...`.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 120s ./internal/memory/drivers/sqlite/... >/dev/null 2>&1; then
    ok 'phase 25: internal/memory/drivers/sqlite tests pass under -race (conformance + migrations + concurrent)'
else
    fail 'phase 25: internal/memory/drivers/sqlite tests failed (run `go test -race ./internal/memory/drivers/sqlite/...` for detail)'
fi

if go test -race -count=1 -timeout 120s ./internal/memory/drivers/postgres/... >/dev/null 2>&1; then
    ok 'phase 25: internal/memory/drivers/postgres tests pass under -race (skip-clean without HARBOR_PG_DSN)'
else
    fail 'phase 25: internal/memory/drivers/postgres tests failed (run `go test -race ./internal/memory/drivers/postgres/...` for detail)'
fi

skip "phase 25: memory drivers have no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
