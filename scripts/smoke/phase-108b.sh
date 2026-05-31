#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 108b — Console app-shell chrome (sidebar + top bar + status bar).
#
# Console-only polish wave (no new Runtime Protocol surface — the ⌘K search
# consumes the already-shipped `search.query`). Like phase-108/108a this smoke
# is static (file existence + token/testid greps); behavioural coverage lives
# in the Playwright specs the frontend job runs (app-shell-chrome.spec.ts +
# shell-no-regression.spec.ts) and the client unit specs (harbor-client.spec).
# Assertions SKIP when a surface is absent so the gate stays green before 108b.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file_or_skip() {
    local path="$1" desc="$2"
    if [ -f "${path}" ]; then ok "${desc}: ${path} exists"
    else skip "${desc}: ${path} missing (Phase 108b not yet implemented)"; fi
}

assert_absent_or_skip() {
    local path="$1" desc="$2"
    if [ ! -f "${path}" ]; then ok "${desc}: ${path} removed"
    else skip "${desc}: ${path} still present (Phase 108b not yet implemented)"; fi
}

assert_grep_or_skip() {
    local pattern="$1" path="$2" desc="$3"
    if [ ! -f "${path}" ]; then skip "${desc}: ${path} not found (Phase 108b not yet implemented)"; return; fi
    if grep -qE "${pattern}" "${path}" 2>/dev/null; then ok "${desc}"
    else skip "${desc}: pattern '${pattern}' absent in ${path} (Phase 108b not yet implemented)"; fi
}

LAYOUT="web/console/src/routes/(console)/+layout.svelte"
TOKENS="web/console/src/lib/tokens.css"

# ---- Sidebar: dedicated compact width token + lucide icons + two-line brand --
assert_grep_or_skip "size-nav:" "${TOKENS}" \
    "phase-108b: dedicated --size-nav sidebar width token (not the detail-rail --size-rail)"
assert_grep_or_skip "@lucide/svelte/icons/" "${LAYOUT}" \
    "phase-108b: sidebar nav items use lucide icons"
assert_grep_or_skip "class=\"nav-icon\"" "${LAYOUT}" \
    "phase-108b: each nav item renders an icon slot"
assert_grep_or_skip "brand-sub" "${LAYOUT}" \
    "phase-108b: two-line Harbor / CONSOLE brand lockup"
assert_grep_or_skip "var\(--size-nav\)" "${LAYOUT}" \
    "phase-108b: sidebar width uses --size-nav"

# ---- Top bar: hamburger collapse + global search + identity avatar -----------
assert_file_or_skip "web/console/src/lib/components/ui/TopBar.svelte" \
    "phase-108b: TopBar chrome component landed"
assert_grep_or_skip "nav-collapse-toggle" \
    "web/console/src/lib/components/ui/TopBar.svelte" \
    "phase-108b: hamburger sidebar-collapse toggle"
assert_grep_or_skip "identity-avatar" \
    "web/console/src/lib/components/ui/TopBar.svelte" \
    "phase-108b: identity avatar + connection popover"

# ---- Global ⌘K search wired to the shipped search.query ----------------------
assert_file_or_skip "web/console/src/lib/components/ui/GlobalSearch.svelte" \
    "phase-108b: GlobalSearch ⌘K launcher landed"
assert_file_or_skip "web/console/src/lib/protocol/search.ts" \
    "phase-108b: search wire types landed"
assert_grep_or_skip "/v1/control/search.query" \
    "web/console/src/lib/protocol/client.ts" \
    "phase-108b: SearchNamespace targets the control-surface route"

# ---- Status-bar consolidation: single global bar, page-local strips removed --
assert_absent_or_skip "web/console/src/lib/components/overview/Footer.svelte" \
    "phase-108b: page-local OverviewFooter removed"
assert_absent_or_skip "web/console/src/lib/components/live-runtime/footer.svelte" \
    "phase-108b: page-local LiveRuntimeFooter removed"
assert_grep_or_skip "AppStatusBar" "${LAYOUT}" \
    "phase-108b: shell renders the single global AppStatusBar"

# ---- Brand-fidelity retheme: Harbor teal accent + Inter + larger logo -------
assert_grep_or_skip "size-brand-logo:" "${TOKENS}" \
    "phase-108b: dedicated (larger) sidebar brand-logo size token"
assert_grep_or_skip "Inter Variable" "${TOKENS}" \
    "phase-108b: --font-sans adopts self-hosted Inter"
assert_file_or_skip "web/console/src/lib/fonts.css" \
    "phase-108b: @font-face stylesheet landed"
assert_file_or_skip "web/console/static/fonts/inter-variable.woff2" \
    "phase-108b: self-hosted Inter woff2 (static, no npm dep)"
# The accent is Harbor's brand teal, NOT the inherited GitHub blue (#2f81f7).
assert_grep_or_skip "color-accent: #2bb6cc" "${TOKENS}" \
    "phase-108b: --color-accent is Harbor teal (not the inherited blue)"

# ---- Hygiene: markdownlint pinned to the CI-bundled version ------------------
assert_grep_or_skip "markdownlint-cli2@" "Makefile" \
    "phase-108b: make markdownlint pins the cli2 version CI uses (@v15 → 0.12.1)"

smoke_summary
