#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 83p — Settings two-group layout closes Bug F1 from the
# post-83k visual walkthrough. The Connected Runtimes form must be
# reachable in the disconnected state (it's the operator's ONLY path
# to attach a Runtime). D-158.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# State file gains the group discriminator + the two helpers.
# ----------------------------------------------------------------------------
assert_grep_present "group: 'console-local'" \
    "web/console/src/lib/settings/state.svelte.ts" \
    "SETTINGS_SECTIONS entries carry the 'console-local' group tag"
assert_grep_present "group: 'runtime-posture'" \
    "web/console/src/lib/settings/state.svelte.ts" \
    "SETTINGS_SECTIONS entries carry the 'runtime-posture' group tag"
assert_grep_present 'export function consoleLocalSections' \
    "web/console/src/lib/settings/state.svelte.ts" \
    "consoleLocalSections helper exported"
assert_grep_present 'export function runtimePostureSections' \
    "web/console/src/lib/settings/state.svelte.ts" \
    "runtimePostureSections helper exported"

# ----------------------------------------------------------------------------
# Page template — split the cards loop into the two-group layout.
# ----------------------------------------------------------------------------
assert_grep_present 'settings-cards-console-local' \
    "web/console/src/routes/(console)/settings/+page.svelte" \
    "Settings page renders the console-local group (outside PageState)"
assert_grep_present 'settings-cards-runtime-posture' \
    "web/console/src/routes/(console)/settings/+page.svelte" \
    "Settings page renders the runtime-posture group (inside PageState)"
assert_grep_present 'visibleConsoleLocal' \
    "web/console/src/routes/(console)/settings/+page.svelte" \
    "page derives the visibleConsoleLocal subset"
assert_grep_present 'visibleRuntimePosture' \
    "web/console/src/routes/(console)/settings/+page.svelte" \
    "page derives the visibleRuntimePosture subset"

# ----------------------------------------------------------------------------
# Playwright test exercises the disconnected-add-form path.
# ----------------------------------------------------------------------------
assert_grep_present 'add a runtime when disconnected' \
    "web/console/tests/settings-page.spec.ts" \
    "Playwright test asserts the disconnected-state add-runtime flow"

smoke_summary
