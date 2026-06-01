#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 108g — Console Sessions page rebuilt + fully wired (D-179;
# supersedes the Phase 73c / D-122 placeholder bottom-dock + disabled-bulk
# composition). The detail bottom-dock tabs render real session-filtered
# event data (not blurb placeholders); the list's bulk Cancel / Pause are
# wired to the shipped cancel / pause control methods (not permanently
# disabled). No Cost column on the list (no shipped per-session cost
# aggregate — D-179).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

LIST="web/console/src/routes/(console)/sessions/+page.svelte"
DETAIL="web/console/src/routes/(console)/sessions/[id]/+page.svelte"
DOCK="web/console/src/lib/components/sessions/BottomDockTabs.svelte"

# ----------------------------------------------------------------------------
# REMOVED — the placeholders D-122 left behind.
# ----------------------------------------------------------------------------
assert_grep_absent 'Phase 73b' "${LIST}" \
    "list bulk actions no longer carry the 'wired with Phase 73b' disabled placeholder"
assert_grep_absent 'decision → tool → result' "${DOCK}" \
    "bottom-dock no longer renders the placeholder Trajectory blurb"
assert_grep_absent 'panel-blurb' "${DOCK}" \
    "bottom-dock no longer renders static blurb panels"

# ----------------------------------------------------------------------------
# PRESENT — the rebuilt list (carded, lean columns, real bulk control).
# ----------------------------------------------------------------------------
assert_grep_present 'data-testid="sessions-page"' "${LIST}" \
    "list page root keeps the sessions-page testid"
assert_grep_present 'data-testid="catalog-row"' "${LIST}" \
    "list page keeps the catalog-row testid"
assert_grep_present 'data-testid="bulk-cancel"' "${LIST}" \
    "list page keeps the bulk-cancel affordance (now wired)"
assert_grep_present 'runBulkControl' "${LIST}" \
    "list page wires bulk Cancel / Pause through a real control handler"
assert_grep_present 'panel card' "${LIST}" \
    "list page adopts the carded .panel.card vocabulary"

# ----------------------------------------------------------------------------
# PRESENT — the rebuilt detail + real bottom dock.
# ----------------------------------------------------------------------------
assert_grep_present 'data-testid="session-detail-page"' "${DETAIL}" \
    "detail page root keeps the session-detail-page testid"
assert_grep_present 'BottomDockTabs' "${DETAIL}" \
    "detail page renders the BottomDockTabs"
assert_grep_present 'EventsSubscription' "${DOCK}" \
    "bottom-dock subscribes to the shipped events.subscribe SSE"
assert_grep_present 'dock-tab-' "${DOCK}" \
    "bottom-dock keeps the five-tab strip (templated dock-tab-<key> testids)"
assert_grep_present "key: 'trajectory'" "${DOCK}" \
    "bottom-dock renders the Trajectory tab"
assert_grep_present "key: 'interventions'" "${DOCK}" \
    "bottom-dock renders the Interventions tab"
assert_grep_present 'projectTrajectory' "${DOCK}" \
    "bottom-dock projects the planner trajectory from the event stream"
assert_grep_present 'projectCost' "${DOCK}" \
    "bottom-dock sums llm.cost.recorded for the Cost History tab"

smoke_summary
