#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 109b smoke — Console MCP Apps host (sandboxed iframe + AppBridge + inline mode).
#
# Classification: static-only — pure file-existence + text greps over web/console
# source. Runs in the parallel batch BEFORE the dev server boots; touches no
# network endpoint.
#
# When the phase ships, replace the `skip` below with the real assertions
# (examples are commented out underneath it).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Phase 109b assertions.
#
# Until the renderer lands, keep preflight green with a single skip. When the
# phase implements its surface, delete the skip and enable the assertions below.
# ----------------------------------------------------------------------------

skip "phase 109b: not yet implemented — Console MCP Apps iframe host + AppBridge + inline mode"

# RENDERER="web/console/src/lib/chat/renderers/mcp-app.svelte"
#
# # The MCP Apps renderer exists in the shared chat module (D-091).
# assert_file_exists "${RENDERER}" "mcp-app.svelte renderer present"
#
# # The renderer sandboxes the iframe, sets a CSP, and validates postMessage origin.
# assert_file_contains "${RENDERER}" "sandbox" "iframe sandbox attribute present"
# assert_file_contains "${RENDERER}" "Content-Security-Policy" "strict CSP present"
# assert_file_contains "${RENDERER}" "origin" "postMessage origin check present"
#
# # Token-surface rule (CLAUDE.md §4.5): no raw color/spacing literals in the new .svelte file.
# assert_no_raw_literals "${RENDERER}" "renderer uses design tokens, not raw literals"

smoke_summary
