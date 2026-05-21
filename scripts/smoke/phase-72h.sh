#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 72h smoke — Console DB local schema + SvelteKit scaffold (D-061,
# D-091, D-092, D-093).
#
# 72h is a frontend-only phase (`web/console/`). No live-server surface to
# exercise; the smoke confirms the Console DB schema module + the SvelteKit
# scaffold files exist, and defence-in-depths the §13 / D-061 carve-out by
# scanning the `TABLE_NAMES` registry in `schema.ts` for forbidden
# runtime-entity table names. Until the phase lands, every file-existence
# check skips per the §4.2 file-absence → SKIP convention.

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
INDEX="web/console/src/lib/db/index.ts"

# ---- Console DB module ------------------------------------------------------
if [ -f "${SCHEMA}" ]; then
    ok "phase-72h: Console DB schema module exists (${SCHEMA})"
    # The eight V1 tables must all be named in schema.ts.
    for table in saved_filters saved_views profiles runtime_registry \
                 auth_profiles pat_store notifications_routing keybindings; do
        if grep -q "'${table}'" "${SCHEMA}"; then
            ok "phase-72h: schema declares table '${table}'"
        else
            fail "phase-72h: schema is missing table '${table}'"
        fi
    done
    # §13 / D-061 carve-out: forbidden runtime-entity table names must
    # never appear in the TABLE_NAMES registry. `LIST_PAGES` legitimately
    # contains page-enum values like 'agents' (the Console renders an
    # Agents *page* from Protocol data) — so the scan extracts ONLY the
    # `export const TABLE_NAMES = [ ... ] as const;` block.
    tbl_block="$(awk '/export const TABLE_NAMES/{f=1} f{print} /\] as const;/{if(f)exit}' "${SCHEMA}")"
    for forbidden in agents sessions tasks tools events artifacts \
                     messages traces runs metrics; do
        if printf '%s' "${tbl_block}" | grep -q "'${forbidden}'"; then
            fail "phase-72h: TABLE_NAMES declares forbidden runtime-entity table '${forbidden}' (D-061)"
        else
            ok "phase-72h: TABLE_NAMES has no '${forbidden}' table (D-061)"
        fi
    done
else
    skip "phase-72h: ${SCHEMA} not yet created (phase pending)"
fi

if [ -f "${MIGRATIONS}" ]; then
    ok "phase-72h: forward-only migrations module exists (${MIGRATIONS})"
    # CLAUDE.md §9: migrations are forward-only. Forbidden destructive
    # shapes must not appear in the migration list.
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

if [ -f "${INDEX}" ]; then
    ok "phase-72h: Console DB public API exists (${INDEX})"
else
    skip "phase-72h: ${INDEX} not yet created (phase pending)"
fi

# ---- SvelteKit scaffold (audit-resolved A5) --------------------------------
SCAFFOLD=(
    "web/console/package.json"
    "web/console/tsconfig.json"
    "web/console/svelte.config.js"
    "web/console/vite.config.ts"
    "web/console/.stylelintrc.cjs"
    "web/console/src/lib/tokens.css"
    "web/console/src/lib/protocol.ts"
    "web/console/src/routes/+layout.svelte"
)
for f in "${SCAFFOLD[@]}"; do
    if [ -f "${f}" ]; then
        ok "phase-72h: scaffold file exists (${f})"
    else
        skip "phase-72h: ${f} not yet created (phase pending)"
    fi
done

# D-092: svelte.config.js must enable runes mode.
if [ -f "web/console/svelte.config.js" ]; then
    if grep -q 'runes: true' "web/console/svelte.config.js"; then
        ok "phase-72h: svelte.config.js enables runes mode (D-092)"
    else
        fail "phase-72h: svelte.config.js missing 'runes: true' (D-092)"
    fi
fi

# D-092: package.json must pin Svelte 5.
if [ -f "web/console/package.json" ]; then
    if grep -q '"svelte": "\^5' "web/console/package.json"; then
        ok "phase-72h: package.json pins svelte ^5 (D-092)"
    else
        fail "phase-72h: package.json must pin svelte ^5 (D-092)"
    fi
fi

# D-093 / D-132 (Wave 13 §17.5 W10): the `cmd/harbor-gen-protocol-ts`
# generator was never built — `protocol.ts` is hand-maintained, NOT
# generated. The checkpoint corrected the formerly-false
# `// CODE GENERATED … DO NOT EDIT` header to an accurate
# "HAND-MAINTAINED" notice (the generator + its CI gate are tracked as a
# post-Wave-13 deliverable). The smoke asserts the accurate header so a
# future regression that re-introduces the false claim is caught.
if [ -f "web/console/src/lib/protocol.ts" ]; then
    if grep -q 'HAND-MAINTAINED' "web/console/src/lib/protocol.ts"; then
        ok "phase-72h: protocol.ts carries the accurate hand-maintained header (D-093 / D-132)"
    else
        fail "phase-72h: protocol.ts missing the hand-maintained header (D-093 / D-132)"
    fi
fi

smoke_summary
