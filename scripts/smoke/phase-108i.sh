#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 108i — Console Tasks page rebuilt to the carded, viewport-locked
# single-page mode-switch (D-181; supersedes the Phase 73d / D-123 pre-chrome
# layout + the placeholder TaskDetailTabs). The page drops the per-page
# PageHeader (the breadcrumb is app-shell chrome), adopts the carded
# `.panel.card` vocabulary, viewport-locks, and wires the per-task bottom dock
# to the live RUN-scoped events.subscribe SSE. No new Protocol method — a pure
# consumer of tasks.list / tasks.get / events.subscribe / pause.list + the
# shipped Phase 54 control verbs.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

PAGE="web/console/src/routes/(console)/tasks/+page.svelte"
DOCK="web/console/src/lib/components/tasks/TaskBottomDock.svelte"
RUNEV="web/console/src/lib/tasks/run-events.ts"
STREAM="web/console/src/lib/tasks/run-stream.svelte.ts"

# ----------------------------------------------------------------------------
# REMOVED — the pre-chrome PageHeader + the shallow placeholder detail tabs.
# ----------------------------------------------------------------------------
assert_grep_absent 'PageHeader' "${PAGE}" \
    "tasks page no longer renders a per-page PageHeader (breadcrumb is chrome)"
assert_grep_absent 'TaskDetailTabs' "${PAGE}" \
    "tasks page no longer imports the placeholder TaskDetailTabs (replaced by TaskBottomDock)"

# ----------------------------------------------------------------------------
# PRESENT — the carded, viewport-locked mode-switch + the live-wired dock.
# ----------------------------------------------------------------------------
assert_grep_present 'data-testid="tasks-page"' "${PAGE}" \
    "tasks page keeps the tasks-page testid"
assert_grep_present 'panel card' "${PAGE}" \
    "tasks page adopts the carded .panel.card vocabulary"
assert_grep_present 'KanbanBoard' "${PAGE}" \
    "tasks page keeps the kanban board (tasks.list rows + aggregates)"
assert_grep_present 'TaskBottomDock' "${PAGE}" \
    "tasks page renders the live run-scoped TaskBottomDock in detail mode"

# The bottom dock renders the per-task tabs over the run-scoped stream.
assert_file "${DOCK}" \
    "TaskBottomDock component exists"
assert_grep_present 'data-testid="task-bottom-dock"' "${DOCK}" \
    "TaskBottomDock renders the per-task dock surface"
assert_grep_present 'TaskRunStream' "${DOCK}" \
    "TaskBottomDock reads the run-scoped TaskRunStream controller"

# The run-scoped subscription lives in the TaskRunStream controller — ONE
# subscription feeds the dock tabs AND the rail cost/event figures.
assert_file "${STREAM}" \
    "TaskRunStream controller exists"
assert_grep_present 'EventsSubscription' "${STREAM}" \
    "TaskRunStream owns a run-scoped events.subscribe subscription"
assert_grep_present 'eventBelongsToRun' "${RUNEV}" \
    "run-events filters the stream to the task's run (run || payload.TaskID || payload.Identity.RunID)"

# The run-match predicate + projections are a pure, unit-tested module.
assert_file "${RUNEV}" \
    "run-events projection module exists"
assert_grep_present 'export function eventBelongsToRun' "${RUNEV}" \
    "run-events exports the eventBelongsToRun predicate"

smoke_summary
