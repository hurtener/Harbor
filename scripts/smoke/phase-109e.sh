#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 109e smoke — MCP App discovery reads the tool-DEFINITION `_meta.ui`
# (spec-conformant), so discovery fires against real ext-apps servers.
#
# Classification: static-only — pure file-existence + text greps against the
# runtime source. Runs in the parallel batch BEFORE the dev server boots. The
# behavioural coverage lives in the Go integration tests
# (internal/tools/drivers/mcp/mcp_app_available_test.go) and the
# HARBOR_LIVE_MCP-gated real-server probe (mcp_live_test.go).
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

MCP_DRIVER="internal/tools/drivers/mcp/mcp.go"
MCP_CONTENT="internal/tools/drivers/mcp/content.go"
APP_TEST="internal/tools/drivers/mcp/mcp_app_available_test.go"
LIVE_TEST="internal/tools/drivers/mcp/mcp_live_test.go"

# ----------------------------------------------------------------------------
# (1) Discovery reads the tool DEFINITION `_meta.ui`, captured at discovery and
#     threaded into the invoke path.
# ----------------------------------------------------------------------------
assert_grep_present 'toolApp := parseAppRef\(t\.Meta\)' "${MCP_DRIVER}" \
    "phase-109e: buildToolDescriptor captures the tool-definition _meta.ui binding"
assert_grep_present 'args json\.RawMessage, toolApp \*AppRef\)' "${MCP_DRIVER}" \
    "phase-109e: callTool takes the tool-definition binding"
assert_grep_present 'value\.AppRef = reconcileAppRef\(toolApp, value\.AppRef, uiDisplayModeHint\(res\.Meta\)\)' "${MCP_DRIVER}" \
    "phase-109e: callTool reconciles the binding with the per-result hint before emit"

# ----------------------------------------------------------------------------
# (2) The reconcile + display-mode-hint helpers (binding is the resourceURI
#     source; the result hint is a secondary merge, never required).
# ----------------------------------------------------------------------------
assert_grep_present 'func reconcileAppRef\(toolBinding, resultHint \*AppRef, resultMode string\) \*AppRef' "${MCP_CONTENT}" \
    "phase-109e: reconcileAppRef merges the tool binding with the optional result hint"
assert_grep_present 'func uiDisplayModeHint\(meta mcpsdk\.Meta\) string' "${MCP_CONTENT}" \
    "phase-109e: uiDisplayModeHint extracts an optional per-result display-mode hint"

# ----------------------------------------------------------------------------
# (3) The CORRECTED fixture: the fake server declares `_meta.ui` on the tool
#     DEFINITION (matching the canonical schema + a real ext-apps server), not
#     on the result. This is the §17.8 mandate that the bug taught.
# ----------------------------------------------------------------------------
assert_grep_present 'Meta:        mcpsdk.Meta{"ui": map\[string\]any{"resourceUri": resourceURI}}' "${APP_TEST}" \
    "phase-109e: the fixture binds _meta.ui.resourceUri on the tool DEFINITION"
assert_grep_present 'func TestE2E_MCPAppAvailable_PlannerPathEmitsEvent' "${APP_TEST}" \
    "phase-109e: the corrected planner-path emit integration test exists"
assert_grep_present 'func TestMCPAppAvailable_ResultHintMergesOverBinding' "${APP_TEST}" \
    "phase-109e: the per-result display-mode merge test exists"
assert_grep_present 'func TestMCPAppAvailable_PlainResultEmitsNoEvent' "${APP_TEST}" \
    "phase-109e: the no-binding negative test exists"

# ----------------------------------------------------------------------------
# (4) The HARBOR_LIVE_MCP-gated real-server probe (CI-skipped regression guard).
# ----------------------------------------------------------------------------
assert_file "${LIVE_TEST}" \
    "phase-109e: the HARBOR_LIVE_MCP real-server probe test landed"
assert_grep_present 'HARBOR_LIVE_MCP' "${LIVE_TEST}" \
    "phase-109e: the live probe is env-gated so CI skips it"

# ----------------------------------------------------------------------------
# (5) Console: a UI-bound tool with no displayMode defaults to inline (the
#     renderer mounts on a bare {resourceUri, serverID}). Skip if the Console
#     is absent from this checkout.
# ----------------------------------------------------------------------------
RENDERER="web/console/src/lib/chat/renderers/mcp-app.svelte"
if [ -f "${RENDERER}" ]; then
    assert_grep_present "app\?\.displayMode \|\| 'inline'" "${RENDERER}" \
        "phase-109e: the Console renderer defaults a UI-bound tool with no mode to inline"
else
    skip "phase-109e: Console renderer absent from this checkout — inline-default assertion skipped"
fi

smoke_summary
