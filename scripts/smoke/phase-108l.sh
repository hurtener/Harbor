#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 108l — Agents fleet-control Protocol surface + the Console Agents page
# rebuilt to the carded, viewport-locked three-column main canvas (D-184;
# supersedes the D-132/F4 control-verb deferral + the Phase 73e / D-124
# pre-chrome layout). The five `agents.*` control verbs are live-server tested
# by scripts/smoke/phase-73e.sh; this script is the STATIC Console-side guard:
# both routes drop the per-page PageHeader, adopt the carded `.panel.card`
# vocabulary, compose the AgentsListPageState / AgentDetailPageState controllers
# + pure derive.ts, wire the control buttons + the live activity feed, and
# preserve the load-bearing testids.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

LIST="web/console/src/routes/(console)/agents/+page.svelte"
DETAIL="web/console/src/routes/(console)/agents/[id]/+page.svelte"
LIST_STATE="web/console/src/lib/agents/state.svelte.ts"
DETAIL_STATE="web/console/src/lib/agents/detail-state.svelte.ts"
DERIVE="web/console/src/lib/agents/derive.ts"
CONTROLS="web/console/src/lib/components/agents/ControlButtons.svelte"
FEED="web/console/src/lib/components/agents/AgentActivityFeed.svelte"
CLIENT="web/console/src/lib/protocol/client.ts"

# ----------------------------------------------------------------------------
# 1. Both routes exist.
# ----------------------------------------------------------------------------
assert_file "${LIST}" "phase 108l: Agents list route exists"
assert_file "${DETAIL}" "phase 108l: Agent detail route exists"

# ----------------------------------------------------------------------------
# 2. REMOVED — the pre-chrome per-page PageHeader (breadcrumb is 108b chrome).
# ----------------------------------------------------------------------------
assert_grep_absent 'PageHeader' "${LIST}" \
    "phase 108l: list route no longer renders a per-page PageHeader"
assert_grep_absent 'PageHeader' "${DETAIL}" \
    "phase 108l: detail route no longer renders a per-page PageHeader"

# ----------------------------------------------------------------------------
# 3. PRESENT — the carded, viewport-locked composition + the page roots.
# ----------------------------------------------------------------------------
assert_grep_present 'data-testid="agents-page"' "${LIST}" \
    "phase 108l: list keeps the agents-page root testid"
assert_grep_present 'data-testid="agent-detail-page"' "${DETAIL}" \
    "phase 108l: detail keeps the agent-detail-page root testid"
assert_grep_present 'panel card' "${LIST}" \
    "phase 108l: list adopts the carded .panel.card vocabulary"
assert_grep_present 'panel card' "${DETAIL}" \
    "phase 108l: detail adopts the carded .panel.card vocabulary"
# The detail is the three-column main canvas, NOT a right rail (page-agents §4).
assert_grep_present 'topology-column' "${DETAIL}" \
    "phase 108l: detail renders the three-column main canvas (topology column)"
assert_grep_present 'activity-column' "${DETAIL}" \
    "phase 108l: detail renders the three-column main canvas (activity column)"

# ----------------------------------------------------------------------------
# 4. The controllers — the king-file refactor targets.
# ----------------------------------------------------------------------------
assert_file "${LIST_STATE}" "phase 108l: AgentsListPageState controller exists"
assert_grep_present 'export class AgentsListPageState' "${LIST_STATE}" \
    "phase 108l: the list controller exports AgentsListPageState"
assert_grep_present 'AgentsListPageState' "${LIST}" \
    "phase 108l: list composes the AgentsListPageState controller"
assert_file "${DETAIL_STATE}" "phase 108l: AgentDetailPageState controller exists"
assert_grep_present 'export class AgentDetailPageState' "${DETAIL_STATE}" \
    "phase 108l: the detail controller exports AgentDetailPageState"
assert_grep_present 'AgentDetailPageState' "${DETAIL}" \
    "phase 108l: detail composes the AgentDetailPageState controller"

# ----------------------------------------------------------------------------
# 5. The pure projections — unit-testable module.
# ----------------------------------------------------------------------------
assert_file "${DERIVE}" "phase 108l: the derive.ts pure-projection module exists"
assert_grep_present 'export function displayStatus' "${DERIVE}" \
    "phase 108l: derive.ts exports displayStatus (ready/empty derived live — D-180)"
assert_grep_present 'export function projectActivity' "${DERIVE}" \
    "phase 108l: derive.ts exports projectActivity (live agent.* projection)"
assert_grep_present 'export function controlResultMessage' "${DERIVE}" \
    "phase 108l: derive.ts exports controlResultMessage (honest re-read status — §13)"
assert_grep_present 'export function toPageError' "${DERIVE}" \
    "phase 108l: derive.ts exports toPageError (no silent swallow — §13)"

# ----------------------------------------------------------------------------
# 6. Fleet control is LIVE + scope-gated (supersedes the D-132/F4 stub).
# ----------------------------------------------------------------------------
# The control buttons must NOT carry the old "no registry.* Protocol surface"
# disabled-stub tooltip — the surface now exists (D-184).
assert_grep_absent 'no registry.\* method exists' "${CONTROLS}" \
    "phase 108l: ControlButtons no longer carries the D-132/F4 'no surface' stub tooltip"
assert_grep_present 'canControl' "${CONTROLS}" \
    "phase 108l: ControlButtons are control-scope gated (canControl)"
assert_grep_present 'onverb' "${CONTROLS}" \
    "phase 108l: ControlButtons invoke the real verb handler (onverb)"
assert_grep_present "hasScope" "${DETAIL_STATE}" \
    "phase 108l: the detail controller gates control on the admin claim (hasScope)"
# The controller invokes the REAL agents.* control methods.
for verb in pause drain restart forceStop deregister; do
    assert_grep_present "${verb}" "${DETAIL_STATE}" \
        "phase 108l: the detail controller wires the real agents.${verb} method"
done
# The client namespace exposes the five control methods.
assert_grep_present '/v1/agents/force_stop' "${CLIENT}" \
    "phase 108l: the agents namespace exposes the force_stop control route"

# ----------------------------------------------------------------------------
# 7. The activity feed is wired live (not the placeholder empty array).
# ----------------------------------------------------------------------------
assert_grep_present 'EventsSubscription' "${DETAIL_STATE}" \
    "phase 108l: the detail controller opens a live agent.* events subscription"
assert_grep_present 'activityEntries' "${DETAIL_STATE}" \
    "phase 108l: the detail controller projects the live activity entries"
assert_grep_present 'data-testid="agent-activity-feed"' "${FEED}" \
    "phase 108l: the activity feed renders its surface"

# ----------------------------------------------------------------------------
# 8. The Save-view contract (phase-83s / disconnected-state N7) is preserved.
# ----------------------------------------------------------------------------
assert_grep_present 'data-testid="agents-save-view"' "${LIST}" \
    "phase 108l: list keeps the agents-save-view button (disconnected-state N7 contract)"
if grep -qE '^[[:space:]]+Save view[[:space:]]*$' "${LIST}"; then
    ok "phase 108l: list renders the 'Save view' button label on its own line (phase-83s N7)"
else
    fail "phase 108l: list must render a 'Save view' button on its own line (phase-83s N7)"
fi
assert_grep_present 'DISCONNECTED_TOOLTIP' "${LIST}" \
    "phase 108l: list gates disconnected controls with the canonical tooltip (phase-83r)"

# ----------------------------------------------------------------------------
# 9. No hand-rolled fetch — Protocol goes through HarborClient (CONVENTIONS §6).
# ----------------------------------------------------------------------------
assert_grep_absent 'fetch(' "${LIST}" \
    "phase 108l: list route has no hand-rolled fetch (HarborClient only)"
assert_grep_absent 'fetch(' "${DETAIL}" \
    "phase 108l: detail route has no hand-rolled fetch (HarborClient only)"

smoke_summary
