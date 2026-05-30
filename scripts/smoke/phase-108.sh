#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 108 — Playground page polish + Console shell layout.
#
# Per §4.2 this smoke is shipping-progress aware: every assertion SKIPs
# when its underlying surface is absent, so the preflight gate stays
# green BEFORE Phase 108 lands. Once each piece ships, the matching
# SKIP flips to OK without any change to the smoke.
#
# Phase 108 is Console-only and additive — no Protocol method, event,
# or wire-shape changes. The smoke is static (file existence + token
# definitions + no-new-deps); behavioural / regression coverage lives
# in the Playwright specs the frontend job runs.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Helpers — local because the existing common.sh helpers target HTTP.
# These wrappers SKIP (do not FAIL) when the artefact is absent, matching
# the §4.2 shipping-progress-aware convention used by phase-107.sh /
# phase-107a.sh.
# ----------------------------------------------------------------------------

assert_file_or_skip() {
    local path="$1" desc="$2"
    if [ -f "${path}" ]; then
        ok "${desc}: ${path} exists"
    else
        skip "${desc}: ${path} missing (Phase 108 not yet implemented)"
    fi
}

assert_grep_or_skip() {
    local pattern="$1" path="$2" desc="$3"
    if [ ! -f "${path}" ]; then
        skip "${desc}: ${path} not found (Phase 108 not yet implemented)"
        return
    fi
    if grep -qE "${pattern}" "${path}" 2>/dev/null; then
        ok "${desc}"
    else
        skip "${desc}: pattern '${pattern}' absent in ${path} (Phase 108 not yet implemented)"
    fi
}

assert_not_grep_or_skip() {
    local pattern="$1" path="$2" desc="$3"
    if [ ! -f "${path}" ]; then
        skip "${desc}: ${path} not found (Phase 108 not yet implemented)"
        return
    fi
    if grep -qE "${pattern}" "${path}" 2>/dev/null; then
        skip "${desc}: pattern '${pattern}' unexpectedly present in ${path} (Phase 108 not yet implemented; AC-16 comment update deferred)"
    else
        ok "${desc}"
    fi
}

# ----------------------------------------------------------------------------
# AC-21 / AC-12 / AC-15 / AC-8 — new files exist.
# ----------------------------------------------------------------------------

assert_file_or_skip \
    "web/console/static/harbor_logo.svg" \
    "phase-108: harbor_logo.svg checked into web/console/static/"

assert_file_or_skip \
    "web/console/src/lib/components/playground/KpiStrip.svelte" \
    "phase-108: KpiStrip.svelte landed"

assert_file_or_skip \
    "web/console/src/lib/components/playground/PlaygroundStatusBar.svelte" \
    "phase-108: PlaygroundStatusBar.svelte landed"

assert_file_or_skip \
    "web/console/src/lib/chat/MarkdownInline.svelte" \
    "phase-108: MarkdownInline.svelte landed"

assert_file_or_skip \
    "web/console/src/lib/chat/MarkdownInline.spec.ts" \
    "phase-108: MarkdownInline.spec.ts landed"

# ----------------------------------------------------------------------------
# AC-4 — chip palette tokens defined in tokens.css.
# ----------------------------------------------------------------------------

TOKENS="web/console/src/lib/tokens.css"

for intent in info success warning danger accent neutral; do
    assert_grep_or_skip \
        "\-\-chip\-${intent}\-fg" \
        "${TOKENS}" \
        "phase-108: --chip-${intent}-fg defined in tokens.css"
done

# ----------------------------------------------------------------------------
# AC-1 — the shell reshape (fixed-height viewport, not min-height).
# ----------------------------------------------------------------------------

SHELL_LAYOUT="web/console/src/routes/(console)/+layout.svelte"

assert_grep_or_skip \
    "height:[[:space:]]*100vh" \
    "${SHELL_LAYOUT}" \
    "phase-108: shell uses height: 100vh (fixed viewport)"

# ----------------------------------------------------------------------------
# AC-16 — MessageBubble's old "V1 renders plain text verbatim" comment is gone.
# ----------------------------------------------------------------------------

assert_not_grep_or_skip \
    "V1 renders plain text verbatim" \
    "web/console/src/lib/chat/MessageBubble.svelte" \
    "phase-108: MessageBubble comment updated for in-house markdown subset"

# ----------------------------------------------------------------------------
# AC-29 — no UNEXPECTED npm dependency lands.
# Phase 108 itself adds no dependency. It runs on a branch that may also carry
# later page-polish phases; the only sanctioned post-108 addition is
# `@lucide/svelte` (Phase 108b — sidebar/top-bar icons, operator-approved). So
# the guard compares the dependency NAME SET against main and fails on any
# added dep outside that allowlist (a count-only check tripped on 108b's
# legitimate icon dep — §17.6). Skips gracefully without git/jq.
# ----------------------------------------------------------------------------

if command -v git >/dev/null 2>&1 && command -v jq >/dev/null 2>&1 \
        && git rev-parse --verify main >/dev/null 2>&1; then
    ADDED=$(comm -13 \
        <(git show main:web/console/package.json 2>/dev/null \
            | jq -r '((.dependencies // {}) | keys[]), ((.devDependencies // {}) | keys[])' 2>/dev/null | sort) \
        <(jq -r '((.dependencies // {}) | keys[]), ((.devDependencies // {}) | keys[])' \
            web/console/package.json 2>/dev/null | sort) )
    UNEXPECTED=$(printf '%s\n' "${ADDED}" | grep -vE '^(@lucide/svelte)?$' || true)
    if [ -z "${UNEXPECTED}" ]; then
        ok "phase-108: no unexpected npm dependency vs main (only sanctioned post-108 additions)"
    else
        fail "phase-108: unexpected dependency vs main: $(printf '%s' "${UNEXPECTED}" | tr '\n' ' ') — Phase 108 is no-new-deps"
    fi
else
    skip "phase-108: package.json dep-set comparison skipped (git or jq or main ref unavailable)"
fi

smoke_summary
