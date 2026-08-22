#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 122 smoke — shared SQL migration runner (internal/persistence/sqlmigrate).
#
# Pure refactor, no behaviour change: the per-driver SQLite + Postgres
# migration runners now delegate to one shared package. The
# no-behaviour-change guarantee is gated by `go test -race` (each driver's
# migration_test.go + the conformance suites); these static assertions pin
# that the dedup actually happened — each driver delegates and no longer
# carries a private runner body — and that searchcache stays deliberately
# OUT of the shared runner (it is divergent: own table, no transactions).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

SM="internal/persistence/sqlmigrate/sqlmigrate.go"

# 1. The shared runner exists with both entry points. RunPostgres remains the
# legacy compatibility helper used by older callers/tests; production
# PostgreSQL projections must consume the namespaced entry point below.
assert_grep_present 'func RunSQLite\(' "${SM}" "phase 122: sqlmigrate.RunSQLite present"
assert_grep_present 'func RunPostgresNamed\(' "${SM}" "phase 122: sqlmigrate.RunPostgresNamed present"
assert_grep_present 'func RunPostgres\(' "${SM}" "phase 122: legacy sqlmigrate.RunPostgres compatibility entry point present"
# The FNV advisory-key derivation is centralised here (one home).
assert_grep_present 'func fnv64aSigned\(' "${SM}" "phase 122: advisory-key derivation lives in sqlmigrate"

# 2. The 4 conformant SQLite drivers delegate + carry no private runner body.
for d in state/drivers/sqlite memory/drivers/sqlite artifacts/drivers/sqlite skills/drivers/localdb; do
    f="internal/${d}/migrations.go"
    assert_grep_present 'sqlmigrate.RunSQLite\(' "${f}" "phase 122: ${d} delegates to sqlmigrate.RunSQLite"
    assert_grep_absent 'func ensureMigrationsTable\(' "${f}" "phase 122: ${d} dropped its private migration runner"
done

# 3. Every Harbor-owned Postgres projection delegates to the namespaced
# runner, binds its closed subsystem identity, and no longer derives the
# advisory key locally. Keep this list exhaustive with the runtime registry:
# state, memory, artifacts, skills, sessions.turns, rollups.
postgres_specs=(
    'state:state/drivers/postgres'
    'memory:memory/drivers/postgres'
    'artifacts:artifacts/drivers/postgres'
    'skills:skills/drivers/postgres'
    'sessions.turns:sessions/turns/drivers/postgres'
    'observability.rollups:observability/rollups/drivers/postgres'
)
for spec in "${postgres_specs[@]}"; do
    subsystem="${spec%%:*}"
    d="${spec#*:}"
    f="internal/${d}/migrations.go"
    assert_grep_present 'sqlmigrate.RunPostgresNamed\(' "${f}" "phase 122: ${d} delegates to sqlmigrate.RunPostgresNamed"
    assert_grep_present "Subsystem:[[:space:]]+\"${subsystem}\"" "${f}" "phase 122: ${d} binds subsystem ${subsystem}"
    assert_grep_absent 'sqlmigrate.RunPostgres\(' "${f}" "phase 122: ${d} does not use the unnamespaced Postgres runner"
    assert_grep_absent 'func fnv64aSigned\(' "${f}" "phase 122: ${d}/postgres no longer derives the advisory key locally"
done

# 4. searchcache is DELIBERATELY excluded (divergent: own table, no txn).
#    It must NOT have been force-fit onto the shared runner.
assert_grep_absent 'sqlmigrate' "internal/tools/drivers/searchcache/migrations.go" \
    "phase 122: searchcache stays standalone (not force-fit onto the shared runner)"

smoke_summary
