#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 83s — saved-views label + footer dedup (N2 / N7).
#
# Static-source assertions only. The live behaviour (single footer per
# page, consistent saved-view labels) is covered by the Playwright spec
# `web/console/tests/disconnected-state.spec.ts` per the §17 contract.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# -----------------------------------------------------------------------------
# N7 — saved-view button label is "Save view" (the canonical verb) on
# every page that exposes a saved-views surface. The pre-83s catalog
# had 4 different verbs ("Save", "Save snapshot", "Save filter",
# "Save view"); the dedup settles on one.
# -----------------------------------------------------------------------------
# A page's saved-view button is rendered as a `<button>` whose VISIBLE
# label is "Save view" on its own line — the grep checks for the literal
# label string (`Save view` plus a trailing newline, anchored on its own
# line). `grep -F` would match the same label appearing inside a comment;
# the trailing-whitespace anchor keeps the match button-shaped.
for page in overview live-runtime sessions tasks agents tools events background-jobs flows memory artifacts; do
    file="web/console/src/routes/(console)/${page}/+page.svelte"
    if grep -qE '^[[:space:]]+Save view[[:space:]]*$' "${file}" 2>/dev/null; then
        ok "${page} page uses 'Save view' as the saved-view button label (N7)"
    else
        fail "${page} page does not render a 'Save view' button on its own line (N7)"
    fi
done

# Playground is a special-case session-level surface; its saved-views is
# also "Save view".
file='web/console/src/routes/(console)/playground/[session_id]/+page.svelte'
if grep -qE '^[[:space:]]+Save view[[:space:]]*$' "${file}" 2>/dev/null; then
    ok 'playground page uses Save view as the saved-view button label (N7)'
else
    fail 'playground page does not render a Save view button on its own line (N7)'
fi

# -----------------------------------------------------------------------------
# N7 — saved-view input placeholder reads "Save current as…" everywhere
# the input is rendered (only pages with a "save name" text input).
# -----------------------------------------------------------------------------
for page in overview live-runtime tasks tools background-jobs; do
    file="web/console/src/routes/(console)/${page}/+page.svelte"
    assert_grep_present \
        'placeholder="Save current as…"' \
        "${file}" \
        "${page} saved-view input placeholder is 'Save current as…' (N7)"
done

# Playground variant.
assert_grep_present \
    'placeholder="Save current as…"' \
    'web/console/src/routes/(console)/playground/[session_id]/+page.svelte' \
    'playground saved-view placeholder is Save current as… (N7)'

# -----------------------------------------------------------------------------
# N2 — no per-page Svelte file renders the ConnectionFooter (the app
# shell renders the single canonical instance). The grep walks the
# route files; the app shell layout is the only legitimate site.
# -----------------------------------------------------------------------------
for page in overview live-runtime sessions tasks agents tools events background-jobs flows memory mcp-connections artifacts settings; do
    file="web/console/src/routes/(console)/${page}/+page.svelte"
    if grep -q '<ConnectionFooter' "${file}" 2>/dev/null; then
        fail "${page} page still renders an inline <ConnectionFooter> (N2 dedup violation)"
    else
        ok "${page} page does not render a duplicate ConnectionFooter (N2)"
    fi
done

# The playground detail route also.
file='web/console/src/routes/(console)/playground/[session_id]/+page.svelte'
if grep -q '<ConnectionFooter' "${file}" 2>/dev/null; then
    fail "playground/[session_id] still renders an inline <ConnectionFooter> (N2 dedup violation)"
else
    ok "playground/[session_id] does not render a duplicate ConnectionFooter (N2)"
fi

# The app shell IS allowed to render it — confirm the single canonical
# render is still there so we don't accidentally delete the only footer.
assert_grep_present \
    '<ConnectionFooter' \
    'web/console/src/routes/(console)/+layout.svelte' \
    'the app shell renders the single ConnectionFooter instance'

smoke_summary
