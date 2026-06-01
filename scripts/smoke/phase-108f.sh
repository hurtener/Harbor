#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 108f — Settings page simplified to the calm "sub-nav + one
# section at a time" model (D-178; supersedes the Phase 73m / D-129
# paginated-cards + saved-views + detail-rail composition). The cruft
# is gone; the D-158 console-local / runtime-posture split is preserved
# per active section.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

PAGE="web/console/src/routes/(console)/settings/+page.svelte"

# ----------------------------------------------------------------------------
# REMOVED — the over-engineered three-nav-models cruft is gone.
# ----------------------------------------------------------------------------
assert_grep_absent 'FilterBar' "${PAGE}" \
    "page no longer imports/uses FilterBar"
assert_grep_absent 'SavedViewChips' "${PAGE}" \
    "page no longer imports/uses SavedViewChips"
assert_grep_absent 'Pagination' "${PAGE}" \
    "page no longer imports/uses Pagination"
assert_grep_absent 'DetailRail' "${PAGE}" \
    "page no longer imports/uses DetailRail"
assert_grep_absent 'settings-save-view' "${PAGE}" \
    "the 'Bookmark section' button + settings-save-view testid is gone"

# ----------------------------------------------------------------------------
# PRESENT — the calm single-section layout.
# ----------------------------------------------------------------------------
assert_grep_present 'data-testid="settings-page"' "${PAGE}" \
    "page root keeps the settings-page testid"
assert_grep_present 'settings-subnav' "web/console/src/lib/components/settings/SubNavRail.svelte" \
    "the sub-nav rail keeps the settings-subnav testid"
assert_grep_present 'SubNavRail' "${PAGE}" \
    "page renders the SubNavRail"
assert_grep_present 'data-testid="settings-active-section"' "${PAGE}" \
    "page renders exactly one active-section heading (single-section model)"

# ----------------------------------------------------------------------------
# PRESERVED — the D-158 console-local / runtime-posture split per section.
# ----------------------------------------------------------------------------
assert_grep_present 'settings-cards-console-local' "${PAGE}" \
    "console-local active section renders directly (works disconnected)"
assert_grep_present 'settings-cards-runtime-posture' "${PAGE}" \
    "runtime-posture active section renders inside PageState"
assert_grep_present 'visibleConsoleLocal' "${PAGE}" \
    "page derives the visibleConsoleLocal subset"
assert_grep_present 'visibleRuntimePosture' "${PAGE}" \
    "page derives the visibleRuntimePosture subset"
assert_grep_present 'AttachToLocalCard' "${PAGE}" \
    "page keeps the AttachToLocalCard import (phase-105 first-attach path)"

smoke_summary
