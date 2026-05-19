#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 72h smoke — Console DB local schema (per D-061).
#
# 72h is a frontend-only schema phase (`web/console/src/lib/db/`). No
# live-server surface to exercise; the smoke confirms the schema files
# exist once the phase ships and defence-in-depths the §13 D-061
# carve-out by scanning `schema.ts` for forbidden runtime-entity table
# names. Until the phase lands, every file-existence check skips per
# the §4.2 404/405/501 → SKIP convention (translated to file-absence
# → SKIP for static-only smokes).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

SCHEMA="web/console/src/lib/db/schema.ts"
MIGRATIONS="web/console/src/lib/db/migrations.ts"
CRYPTO="web/console/src/lib/db/crypto.ts"
DRIVER="web/console/src/lib/db/driver.ts"
DRIVER_IDB="web/console/src/lib/db/drivers/indexeddb.ts"

if [ -f "${SCHEMA}" ]; then
    ok "phase-72h: Console DB schema module exists (${SCHEMA})"
    # §13 D-061 carve-out: forbidden runtime-entity table names must
    # never appear in the Console DB schema. Defence in depth over the
    # in-package schema-carveout.spec.ts.
    assert_grep_absent '"agents"'    "${SCHEMA}" "phase-72h: schema has no 'agents' table (D-061)"
    assert_grep_absent '"sessions"'  "${SCHEMA}" "phase-72h: schema has no 'sessions' table (D-061)"
    assert_grep_absent '"tasks"'     "${SCHEMA}" "phase-72h: schema has no 'tasks' table (D-061)"
    assert_grep_absent '"tools"'     "${SCHEMA}" "phase-72h: schema has no 'tools' table (D-061)"
    assert_grep_absent '"events"'    "${SCHEMA}" "phase-72h: schema has no 'events' table (D-061)"
    assert_grep_absent '"artifacts"' "${SCHEMA}" "phase-72h: schema has no 'artifacts' table (D-061)"
    assert_grep_absent '"messages"'  "${SCHEMA}" "phase-72h: schema has no 'messages' table (D-061)"
    assert_grep_absent '"traces"'    "${SCHEMA}" "phase-72h: schema has no 'traces' table (D-061)"
    assert_grep_absent '"runs"'      "${SCHEMA}" "phase-72h: schema has no 'runs' table (D-061)"
else
    skip "phase-72h: ${SCHEMA} not yet created (phase pending)"
fi

if [ -f "${MIGRATIONS}" ]; then
    ok "phase-72h: forward-only migrations module exists (${MIGRATIONS})"
    # CLAUDE.md §9: migrations are forward-only. Forbidden destructive
    # SQL/JS shapes must not appear in the migration list.
    assert_grep_absent 'DROP TABLE'    "${MIGRATIONS}" "phase-72h: no DROP TABLE in migrations"
    assert_grep_absent 'ALTER COLUMN'  "${MIGRATIONS}" "phase-72h: no ALTER COLUMN in migrations"
else
    skip "phase-72h: ${MIGRATIONS} not yet created (phase pending)"
fi

if [ -f "${CRYPTO}" ]; then
    ok "phase-72h: WebCrypto envelope module exists (${CRYPTO})"
else
    skip "phase-72h: ${CRYPTO} not yet created (phase pending)"
fi

if [ -f "${DRIVER}" ]; then
    ok "phase-72h: ConsoleDB driver interface exists (${DRIVER})"
else
    skip "phase-72h: ${DRIVER} not yet created (phase pending)"
fi

if [ -f "${DRIVER_IDB}" ]; then
    ok "phase-72h: IndexedDB driver exists (${DRIVER_IDB})"
else
    skip "phase-72h: ${DRIVER_IDB} not yet created (phase pending)"
fi

smoke_summary
