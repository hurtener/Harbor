#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 108m — Console MCP Connections page rethemed to the carded,
# viewport-locked master-detail composition + the right rail deepened to full
# mock fidelity + the king file refactored to controllers + pure derive.ts
# (D-185; supersedes the Phase 73k / D-119 pre-chrome layout). This is a PURE
# Console phase — the `mcp.servers.*` surface shipped in Phase 73k (live-server
# tested elsewhere); this script is the STATIC Console-side guard: the page
# drops the per-page PageHeader, adopts the carded `.panel.card` vocabulary,
# composes the McpListState / McpDetailState controllers + pure derive.ts,
# wires the real control actions + the live recent-events subscription,
# replaces the separate tabbed-detail route with the right rail, and preserves
# the load-bearing testids.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

PAGE="web/console/src/routes/(console)/mcp-connections/+page.svelte"
DETAIL_ROUTE="web/console/src/routes/(console)/mcp-connections/[server]"
STATE="web/console/src/lib/mcp-connections/state.svelte.ts"
DERIVE="web/console/src/lib/mcp-connections/derive.ts"
STATUS="web/console/src/lib/mcp-connections/status.ts"
TABLE="web/console/src/lib/components/mcp-connections/ServersTable.svelte"
RAIL="web/console/src/lib/components/mcp-connections/McpDetailRail.svelte"
OVERVIEW="web/console/src/lib/components/mcp-connections/McpOverviewCard.svelte"
EVENTS="web/console/src/lib/components/mcp-connections/McpRecentEvents.svelte"

# ----------------------------------------------------------------------------
# 1. The list route exists; the separate tabbed-detail route is GONE (the rail
#    is the single detail surface — §13, no two parallel implementations).
# ----------------------------------------------------------------------------
assert_file "${PAGE}" "phase 108m: MCP Connections page route exists"
if [ -d "${DETAIL_ROUTE}" ]; then
    fail "phase 108m: the separate [server] detail route must be removed (rail is the single detail surface)"
else
    ok "phase 108m: the separate [server] detail route is removed (master-detail in the rail)"
fi

# ----------------------------------------------------------------------------
# 2. REMOVED — the pre-chrome per-page PageHeader (breadcrumb is 108b chrome).
# ----------------------------------------------------------------------------
assert_grep_absent 'PageHeader' "${PAGE}" \
    "phase 108m: the page no longer renders a per-page PageHeader"

# ----------------------------------------------------------------------------
# 3. PRESENT — the carded, viewport-locked composition + the page root testid.
# ----------------------------------------------------------------------------
assert_grep_present 'data-testid="mcp-connections-list"' "${PAGE}" \
    "phase 108m: the page keeps the mcp-connections-list root testid"
assert_grep_present 'panel card' "${PAGE}" \
    "phase 108m: the page adopts the carded .panel.card vocabulary"
assert_grep_present 'data-testid="mcp-search"' "${PAGE}" \
    "phase 108m: the page keeps the mcp-search FilterBar input testid"
assert_grep_present 'data-testid="list-empty"' "${PAGE}" \
    "phase 108m: the page keeps the list-empty state testid"

# ----------------------------------------------------------------------------
# 4. The controllers — the king-file refactor targets.
# ----------------------------------------------------------------------------
assert_file "${STATE}" "phase 108m: the state controller module exists"
assert_grep_present 'export class McpListState' "${STATE}" \
    "phase 108m: the module exports the McpListState list controller"
assert_grep_present 'export class McpDetailState' "${STATE}" \
    "phase 108m: the module exports the McpDetailState detail controller"
assert_grep_present 'McpListState' "${PAGE}" \
    "phase 108m: the page composes the McpListState controller"
assert_grep_present 'McpDetailState' "${PAGE}" \
    "phase 108m: the page composes the McpDetailState controller"

# ----------------------------------------------------------------------------
# 5. The pure projections — unit-testable module (status.ts folded in).
# ----------------------------------------------------------------------------
assert_file "${DERIVE}" "phase 108m: the derive.ts pure-projection module exists"
if [ -f "${STATUS}" ]; then
    fail "phase 108m: status.ts must be folded into derive.ts (one projection home)"
else
    ok "phase 108m: status.ts is folded into derive.ts (one projection home)"
fi
assert_grep_present 'export function mcpStatusKind' "${DERIVE}" \
    "phase 108m: derive.ts exports mcpStatusKind (folded in from status.ts)"
assert_grep_present 'export function displayStatus' "${DERIVE}" \
    "phase 108m: derive.ts exports displayStatus (ready/empty derived live — D-180)"
assert_grep_present 'export function projectServerEvents' "${DERIVE}" \
    "phase 108m: derive.ts exports projectServerEvents (live recent-event projection)"
assert_grep_present 'export function toPageError' "${DERIVE}" \
    "phase 108m: derive.ts exports toPageError (no silent swallow — §13)"

# ----------------------------------------------------------------------------
# 6. The focused components — the master-detail split.
# ----------------------------------------------------------------------------
assert_file "${TABLE}" "phase 108m: the ServersTable component exists"
assert_file "${RAIL}" "phase 108m: the McpDetailRail component exists"
assert_file "${OVERVIEW}" "phase 108m: the McpOverviewCard idle-state component exists"
assert_file "${EVENTS}" "phase 108m: the McpRecentEvents live-feed component exists"
assert_grep_present 'data-testid="rail-server-name"' "${RAIL}" \
    "phase 108m: the rail keeps the rail-server-name testid"

# ----------------------------------------------------------------------------
# 7. The deepened rail real-wires the actions (§13 — no fabrication).
# ----------------------------------------------------------------------------
assert_grep_present 'MCP_DETAIL_TABS' "${RAIL}" \
    "phase 108m: the rail composes the MCP_DETAIL_TABS tab strip"
for tab in tools resources prompts oauth policy; do
    assert_grep_present "id: '${tab}'" "${STATE}" \
        "phase 108m: MCP_DETAIL_TABS defines the ${tab} tab"
done
assert_grep_present 'data-testid="refresh-discovery"' "${RAIL}" \
    "phase 108m: the rail wires Refresh discovery"
assert_grep_present 'data-testid="test-connection"' "${RAIL}" \
    "phase 108m: the rail wires Test connection (real probe outcome)"
assert_grep_present 'data-testid="raw-html-toggle"' "${RAIL}" \
    "phase 108m: the rail wires the admin raw-HTML trust toggle"
assert_grep_present 'isAdmin' "${RAIL}" \
    "phase 108m: the rail admin-gates the raw-HTML toggle + OAuth verbs (isAdmin)"
assert_grep_present 'tools-deep-link' "${RAIL}" \
    "phase 108m: the Tools tab deep-links to the Tools page (no parallel OAuth path)"
# The detail controller wires the real mcp.servers.* verbs + the live stream.
assert_grep_present 'hasScope' "${STATE}" \
    "phase 108m: the detail controller derives the admin claim (hasScope)"
assert_grep_present 'EventsSubscription' "${STATE}" \
    "phase 108m: the detail controller opens a live recent-events subscription"
for verb in refreshDiscovery probe setRawHTMLTrust revokeBinding; do
    assert_grep_present "${verb}" "${STATE}" \
        "phase 108m: the detail controller wires the real mcp.servers ${verb} verb"
done

# ----------------------------------------------------------------------------
# 8. The Save-view contract (phase-83s / disconnected-state N7) is preserved.
# ----------------------------------------------------------------------------
assert_grep_present 'data-testid="save-view"' "${PAGE}" \
    "phase 108m: the page keeps the save-view button (disconnected-state N7 contract)"
if grep -qE '^[[:space:]]+Save view[[:space:]]*$' "${PAGE}"; then
    ok "phase 108m: the page renders the 'Save view' button label on its own line (phase-83s N7)"
else
    fail "phase 108m: the page must render a 'Save view' button on its own line (phase-83s N7)"
fi
assert_grep_present 'DISCONNECTED_TOOLTIP' "${PAGE}" \
    "phase 108m: the page gates disconnected controls with the canonical tooltip (phase-83r)"

# ----------------------------------------------------------------------------
# 9. No hand-rolled fetch — Protocol goes through HarborClient (CONVENTIONS §6).
# ----------------------------------------------------------------------------
assert_grep_absent 'fetch\(' "${PAGE}" \
    "phase 108m: the page route has no hand-rolled fetch (HarborClient only)"
assert_grep_absent 'fetch\(' "${STATE}" \
    "phase 108m: the controller has no hand-rolled fetch (HarborClient only)"

smoke_summary
