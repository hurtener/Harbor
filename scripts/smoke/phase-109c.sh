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
# Phase 109c assertions go below. When the layout lands, replace the skip with
# real file-existence + text greps. Example assertions (commented until ship):
#
#   PLAYGROUND_PAGE="web/console/src/routes/(console)/playground/[session_id]/+page.svelte"
#   APP_PANEL="web/console/src/lib/components/playground/AppPanel.svelte"
#   TAB_STRIP="web/console/src/lib/components/playground/AppTabStrip.svelte"
#   SPLIT_PANE="web/console/src/lib/components/playground/SplitPane.svelte"
#
#   # The new layout components exist.
#   assert_file_exists "${APP_PANEL}"  "AppPanel component exists"
#   assert_file_exists "${TAB_STRIP}"  "AppTabStrip component exists"
#   assert_file_exists "${SPLIT_PANE}" "SplitPane component exists"
#
#   # The Playground page references all three DisplayMode branches.
#   assert_file_contains "${PLAYGROUND_PAGE}" "fullscreen" "page handles fullscreen DisplayMode"
#   assert_file_contains "${PLAYGROUND_PAGE}" "pip"        "page handles pip DisplayMode"
#   assert_file_contains "${PLAYGROUND_PAGE}" "inline"     "page handles inline DisplayMode"
#
#   # No raw color/spacing literals in the new .svelte files (token-surface rule, §4.5).
#   assert_no_raw_literals "${APP_PANEL}" "${TAB_STRIP}" "${SPLIT_PANE}" \
#     "new playground layout components use tokens only"
# ----------------------------------------------------------------------------

skip "phase 109c: not yet implemented — MCP Apps fullscreen/pip DisplayMode layout"

smoke_summary
