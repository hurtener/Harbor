#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 108j — Console Background Jobs page rebuilt to the carded,
# viewport-locked Events-108h composition (D-182; supersedes the Phase 73h /
# D-128 pre-chrome layout). The page drops the per-page PageHeader (the
# breadcrumb is app-shell chrome), adopts the carded `.panel.card` vocabulary,
# viewport-locks (TABLE-primary + a right-rail detail), deepens the rail to full
# mock fidelity (Details / Progress / Events / Control History + Artifacts /
# Parent task / Related Sessions), and refactors the ~801-line king file into a
# BackgroundJobsPageState controller + pure derive.ts / orphan-detector.ts
# projections + focused rail components, reusing the Tasks-108i TaskRunStream /
# run-events data layer. No new Protocol method — a pure consumer of tasks.list /
# tasks.get / artifacts.list / events.subscribe + the shipped Phase 54 verbs.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

PAGE="web/console/src/routes/(console)/background-jobs/+page.svelte"
STATE="web/console/src/lib/background-jobs/state.svelte.ts"
DERIVE="web/console/src/lib/background-jobs/derive.ts"
DETECTOR="web/console/src/lib/background-jobs/orphan-detector.ts"
RAIL="web/console/src/lib/components/background-jobs/JobDetailRail.svelte"
PROGRESS="web/console/src/lib/components/background-jobs/JobProgressTab.svelte"
QUEUE="web/console/src/lib/components/background-jobs/QueueTable.svelte"
OLD_RAIL="web/console/src/lib/components/background-jobs/RightRail.svelte"

# ----------------------------------------------------------------------------
# 1. Route exists.
# ----------------------------------------------------------------------------
assert_file "${PAGE}" "phase 108j: Background Jobs route exists"

# ----------------------------------------------------------------------------
# 2. REMOVED — the pre-chrome per-page PageHeader + the superseded RightRail.
# ----------------------------------------------------------------------------
assert_grep_absent 'PageHeader' "${PAGE}" \
    "phase 108j: Background Jobs page no longer renders a per-page PageHeader (breadcrumb is chrome)"
if [[ -f "${OLD_RAIL}" ]]; then
    fail "phase 108j: the superseded RightRail.svelte should be deleted (replaced by JobDetailRail.svelte)"
else
    ok "phase 108j: the pre-chrome RightRail.svelte is deleted (replaced by JobDetailRail.svelte)"
fi

# ----------------------------------------------------------------------------
# 3. PRESENT — the carded, viewport-locked composition + the page root.
# ----------------------------------------------------------------------------
assert_grep_present 'data-testid="background-jobs-page"' "${PAGE}" \
    "phase 108j: page keeps the background-jobs-page root testid"
assert_grep_present 'panel card' "${PAGE}" \
    "phase 108j: page adopts the carded .panel.card vocabulary"
assert_grep_present 'BackgroundJobsPageState' "${PAGE}" \
    "phase 108j: page composes the BackgroundJobsPageState controller (king-file refactor)"

# ----------------------------------------------------------------------------
# 4. The controller — the king-file refactor target.
# ----------------------------------------------------------------------------
assert_file "${STATE}" "phase 108j: BackgroundJobsPageState controller exists"
assert_grep_present 'export class BackgroundJobsPageState' "${STATE}" \
    "phase 108j: the controller exports the BackgroundJobsPageState class"
# Reuses the Tasks-108i run-scoped data layer (NOT forked).
assert_grep_present 'run-stream.svelte' "${STATE}" \
    "phase 108j: the controller reuses the Tasks-108i TaskRunStream (run-stream.svelte) — not forked"
# Artifacts-so-far is wired to the shipped artifacts.list (scope.task = run id).
assert_grep_present 'artifacts.list' "${STATE}" \
    "phase 108j: the controller wires Artifacts-so-far to the shipped artifacts.list"

# ----------------------------------------------------------------------------
# 5. The pure projections — unit-testable modules.
# ----------------------------------------------------------------------------
assert_file "${DERIVE}" "phase 108j: the derive.ts pure-projection module exists"
assert_grep_present 'export function deriveETA' "${DERIVE}" \
    "phase 108j: derive.ts exports deriveETA (ETA from the planner progress hint / Unknown)"
assert_grep_present 'export function deriveJobType' "${DERIVE}" \
    "phase 108j: derive.ts exports deriveJobType (tags/description / Job)"
assert_grep_present 'export function projectStateTimeline' "${DERIVE}" \
    "phase 108j: derive.ts exports projectStateTimeline (task.* lifecycle timeline)"
assert_file "${DETECTOR}" "phase 108j: the orphan-detector module is kept"
assert_grep_present 'export function detectOrphans' "${DETECTOR}" \
    "phase 108j: orphan-detector exports the pure detectOrphans cross-check (D-128)"

# ----------------------------------------------------------------------------
# 6. The deepened right rail — full mock fidelity.
# ----------------------------------------------------------------------------
assert_file "${RAIL}" "phase 108j: the JobDetailRail component exists"
assert_grep_present 'data-testid="bg-right-rail"' "${RAIL}" \
    "phase 108j: JobDetailRail renders the per-job detail surface"
# The four rail tabs are rendered from a TABS array via a template testid
# (`bg-rail-tab-${tab.key}`) — anchor on the template prefix + the tab keys,
# never on a per-tab literal (which a templated data-testid never emits).
assert_grep_present 'bg-rail-tab-' "${RAIL}" \
    "phase 108j: JobDetailRail renders the rail tab strip (templated bg-rail-tab- testid)"
for key in details progress events control; do
    assert_grep_present "'${key}'" "${RAIL}" \
        "phase 108j: JobDetailRail declares the ${key} rail tab"
done
assert_file "${PROGRESS}" "phase 108j: the JobProgressTab component exists"
assert_grep_present 'deriveETA' "${PROGRESS}" \
    "phase 108j: JobProgressTab renders the derived ETA (honest Unknown without a hint)"

# ----------------------------------------------------------------------------
# 7. The queue table — short id + derived type badge + the orphan badge.
# ----------------------------------------------------------------------------
assert_grep_present 'data-testid="bg-job-row"' "${QUEUE}" \
    "phase 108j: QueueTable renders the bg-job-row rows"
assert_grep_present 'data-testid="bg-job-type"' "${QUEUE}" \
    "phase 108j: QueueTable renders the derived type badge (honest — never a fabricated agent)"
assert_grep_present 'deriveJobType' "${QUEUE}" \
    "phase 108j: QueueTable derives the type badge from tags/description"

# ----------------------------------------------------------------------------
# 8. The Save-view contract (phase-83s / disconnected-state N7) is preserved.
# ----------------------------------------------------------------------------
assert_grep_present 'data-testid="bg-save-filter"' "${PAGE}" \
    "phase 108j: page keeps the bg-save-filter button (disconnected-state N7 contract)"
assert_grep_present 'placeholder="Save current as…"' "${PAGE}" \
    "phase 108j: page keeps the 'Save current as…' saved-view input (phase-83s N7)"
if grep -qE '^[[:space:]]+Save view[[:space:]]*$' "${PAGE}"; then
    ok "phase 108j: page renders the 'Save view' button label on its own line (phase-83s N7)"
else
    fail "phase 108j: page must render a 'Save view' button on its own line (phase-83s N7)"
fi
assert_grep_present 'DISCONNECTED_TOOLTIP' "${PAGE}" \
    "phase 108j: page gates disconnected controls with the canonical tooltip (phase-83r)"

smoke_summary
