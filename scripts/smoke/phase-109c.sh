#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 109c smoke — MCP Apps fullscreen/pip DisplayMode layout (Console-side).
#
# Classification: static-only — pure file-existence + text greps against
# web/console/ source. Runs in the parallel batch BEFORE the dev server boots.
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Phase 109c assertions — static file-existence + text greps against the
# Console source. Behavioural coverage lives in the vitest unit suite
# (src/lib/components/playground/layout.spec.ts) and the Playwright layout
# suite (tests/mcp-app-displaymode.spec.ts) the frontend job runs.
# ----------------------------------------------------------------------------

PLAYGROUND_PAGE="web/console/src/routes/(console)/playground/[session_id]/+page.svelte"
LAYOUT="web/console/src/lib/components/playground/layout.ts"
APP_PANEL="web/console/src/lib/components/playground/AppPanel.svelte"
TAB_STRIP="web/console/src/lib/components/playground/AppTabStrip.svelte"
SPLIT_PANE="web/console/src/lib/components/playground/SplitPane.svelte"
LAYOUT_SPEC="web/console/src/lib/components/playground/layout.spec.ts"
PW_SPEC="web/console/tests/mcp-app-displaymode.spec.ts"

skip_if_absent() {
    # If the Playground page itself is missing, the Console source tree is not
    # present in this checkout — SKIP the whole phase (keeps the gate green on
    # non-Console builds). Returns 0 to continue, 1 to short-circuit.
    if [ ! -f "${PLAYGROUND_PAGE}" ]; then
        skip "phase 109c: ${PLAYGROUND_PAGE} absent (Console source not in this checkout)"
        return 1
    fi
    return 0
}

if skip_if_absent; then
    # ---- The pure state machine + the three components landed ----------------
    assert_file "${LAYOUT}" "phase-109c: the layout state machine landed"
    assert_file "${APP_PANEL}" "phase-109c: AppPanel component landed"
    assert_file "${TAB_STRIP}" "phase-109c: AppTabStrip component landed"
    assert_file "${SPLIT_PANE}" "phase-109c: SplitPane component landed"
    assert_file "${LAYOUT_SPEC}" "phase-109c: the layout unit suite landed"
    assert_file "${PW_SPEC}" "phase-109c: the Playwright DisplayMode layout suite landed"

    # ---- The state machine exports the pure (DisplayMode, openApps) → region --
    assert_grep_present "export function computeRegion" "${LAYOUT}" \
        "phase-109c: layout.ts exports the pure region projection"
    assert_grep_present "export function reduceLayout" "${LAYOUT}" \
        "phase-109c: layout.ts exports the pure reducer"
    assert_grep_present "clampRatio" "${LAYOUT}" \
        "phase-109c: layout.ts clamps the split ratio to sane bounds"

    # ---- The page routes the three DisplayMode regions -----------------------
    assert_grep_present "region.region === 'fullscreen'" "${PLAYGROUND_PAGE}" \
        "phase-109c: the page routes the fullscreen region"
    assert_grep_present "region.region === 'pip'" "${PLAYGROUND_PAGE}" \
        "phase-109c: the page routes the pip region"
    assert_grep_present "computeRegion" "${PLAYGROUND_PAGE}" \
        "phase-109c: the page derives its region from the layout state machine"

    # ---- fullscreen = tab strip; pip = split + rail toggle -------------------
    assert_grep_present "AppTabStrip" "${PLAYGROUND_PAGE}" \
        "phase-109c: fullscreen renders the tab strip"
    assert_grep_present "SplitPane" "${PLAYGROUND_PAGE}" \
        "phase-109c: pip renders the resizable split"
    assert_grep_present "pip-rail-toggle" "${PLAYGROUND_PAGE}" \
        "phase-109c: pip hides the rail by default with a toggle to reopen it"

    # ---- The renderer is REUSED, not forked (registry seam) ------------------
    assert_grep_present "dispatchRenderer\(MCP_APP_INLINE_MIME\)" "${APP_PANEL}" \
        "phase-109c: AppPanel reuses the 109b renderer via the registry seam (no fork)"

    # ---- No raw color/spacing literals in the new .svelte <style> blocks -----
    for f in "${APP_PANEL}" "${TAB_STRIP}" "${SPLIT_PANE}"; do
        if [ -f "${f}" ]; then
            style_block="$(awk '/<style>/{s=1} s{print} /<\/style>/{s=0}' "${f}")"
            if printf '%s' "${style_block}" | grep -qE '#[0-9a-fA-F]{3,8}|rgba?\(|[0-9]+px|[0-9]*\.?[0-9]+rem'; then
                fail "phase-109c: raw color/spacing literal in ${f} <style> (use tokens — §4.5)"
            else
                ok "phase-109c: ${f} <style> uses tokens only, no raw literals"
            fi
        fi
    done
fi

smoke_summary
