#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 108h — Console Events page rethemed to the carded, viewport-locked
# composition (D-180; refines the Phase 73g / D-125 pre-chrome layout). The
# page drops the per-page PageHeader (the breadcrumb is app-shell chrome),
# adopts the carded `.panel.card` vocabulary, viewport-locks, and fills the
# idle right-rail subscription-status gap. The data layer (EventsPageState +
# the events lib) is unchanged; no new Protocol method.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

PAGE="web/console/src/routes/(console)/events/+page.svelte"

# ----------------------------------------------------------------------------
# REMOVED — the pre-chrome PageHeader.
# ----------------------------------------------------------------------------
assert_grep_absent 'PageHeader' "${PAGE}" \
    "events page no longer renders a per-page PageHeader (breadcrumb is chrome)"

# ----------------------------------------------------------------------------
# PRESENT — the carded, viewport-locked composition + the kept wiring.
# ----------------------------------------------------------------------------
assert_grep_present 'data-testid="events-page"' "${PAGE}" \
    "events page keeps the events-page testid"
assert_grep_present 'panel card' "${PAGE}" \
    "events page adopts the carded .panel.card vocabulary"
assert_grep_present 'EventsPageState' "${PAGE}" \
    "events page keeps the EventsPageState controller (live events.subscribe feed)"
assert_grep_present 'EventRateSparkline' "${PAGE}" \
    "events page keeps the events.aggregate rate sparkline"
assert_grep_present 'EventDetailRail' "${PAGE}" \
    "events page keeps the Event Details right rail"
assert_grep_present 'data-testid="subscription-status"' "${PAGE}" \
    "events page renders the idle right-rail subscription status (cursor / dropped / stream state)"

smoke_summary
