#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 83x — real-data-layout-bugs (W4-W11 + N11-N14).
#
# Static-source tripwires for the twelve per-page polish fixes. Each
# `assert_grep_present` pins a load-bearing string / shape a future
# refactor cannot silently delete. The live behaviour (column rendering,
# kanban transitions, label suffixes) is covered by the Playwright
# walkthrough rerun after the wave-round-2 PR lands per §17.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# -----------------------------------------------------------------------------
# W4 — Memory page MEMORY KEY column renders single-line ellipsis instead
# of per-character vertical wrap.
# -----------------------------------------------------------------------------
assert_grep_present \
    'text-overflow: ellipsis' \
    'web/console/src/routes/(console)/memory/+page.svelte' \
    'W4 Memory page key column uses single-line ellipsis (no per-glyph wrap)'
assert_grep_present \
    'title=\{item.key\}' \
    'web/console/src/routes/(console)/memory/+page.svelte' \
    'W4 Memory page key cell carries a hover-title for full-key reveal'

# -----------------------------------------------------------------------------
# W5 — Artifacts page right-rail stops overlapping the table (grid layout
# reserves the rail column).
# -----------------------------------------------------------------------------
assert_grep_present \
    'grid-template-columns: 1fr var\(--size-rail\)' \
    'web/console/src/routes/(console)/artifacts/+page.svelte' \
    'W5 Artifacts page catalog uses 1fr + size-rail grid (no overlap)'

# -----------------------------------------------------------------------------
# W6 — Artifacts `created_at` is stamped at promotion time + on uploads.
# Two call sites: the dev tool executor + the Protocol artifacts.put
# handler. Both populate the storage Source map so the projectRow wire
# layer's extractCreatedAt populates a real timestamp.
# -----------------------------------------------------------------------------
assert_grep_present \
    '"created_at": time.Now\(\).UTC\(\)' \
    'cmd/harbor/cmd_dev_executor.go' \
    'W6 dev-tool-executor stamps created_at on heavy-promoted artifacts'
assert_grep_present \
    '"created_at": s.clock\(\)' \
    'internal/protocol/artifacts.go' \
    'W6 artifacts.put handler stamps created_at on uploads'

# -----------------------------------------------------------------------------
# W7 — Tasks kanban carries a Complete column (so completed tasks are
# visible alongside the right-rail summary's Complete counter).
# -----------------------------------------------------------------------------
assert_grep_present \
    "status: 'complete', label: 'Complete'" \
    'web/console/src/lib/protocol/tasks.ts' \
    'W7 Tasks kanban column list includes Complete'
assert_grep_present \
    "case 'complete':" \
    'web/console/src/lib/components/tasks/KanbanBoard.svelte' \
    'W7 KanbanBoard.columnCount switch handles complete'
# Phase 108i (D-181): the board grid uses `repeat(5, minmax(0, 1fr))` so
# the columns can shrink to scroll internally (viewport-lock) — still five
# columns. The pattern matches both the original `1fr` and the `minmax(0,
# 1fr)` form.
assert_grep_present \
    'grid-template-columns: repeat\(5,' \
    'web/console/src/lib/components/tasks/KanbanBoard.svelte' \
    'W7 Kanban grid widened to five columns'

# -----------------------------------------------------------------------------
# W8 (superseded by D-171) — dev boot no longer Opens a fixed dev session
# at boot (that boot-time Open crashed when the persisted "dev" session
# was idle-GC-closed). Sessions are now create-on-first-use via the
# SessionEnsurer, and a persistent catalog backs the Sessions page across
# restarts. Assert the old crash-causing boot Open is GONE and the new
# ensurer wiring is present.
# -----------------------------------------------------------------------------
assert_grep_absent \
    'sessionRegistry.Open\(devSessCtx, DevSession, devID\)' \
    'cmd/harbor/cmd_dev.go' \
    'W8/D-171 dev boot no longer Opens a fixed dev session at boot (crash fix)'
assert_grep_present \
    'WithSessionEnsurer' \
    'cmd/harbor/cmd_dev.go' \
    'W8/D-171 dev wires the SessionEnsurer (create-on-first-use)'

# -----------------------------------------------------------------------------
# W9 — Events page empty-state copy names the `events.driver: durable`
# storage-driver requirement so an operator stops chasing a phantom bug
# under the default `inmem` driver.
# -----------------------------------------------------------------------------
assert_grep_present \
    'events.driver: durable' \
    'web/console/src/routes/(console)/events/+page.svelte' \
    'W9 Events page empty copy documents events.driver: durable'

# -----------------------------------------------------------------------------
# W10 — Live Runtime session-detail Status field is derived from the
# status-counter strip (the live task aggregate), not the page's own
# PageStatus. A topology-snapshot failure no longer poisons the rail.
# Phase 108e reframed the page into the capability cockpit and removed the
# detail rail; the `<SessionDetailCard sessionStatus={sessionStatusLabel}>`
# binding now lives in the Active-sessions cockpit panel. The page still
# DERIVES `sessionStatusLabel` from the strip and passes it down — the W10
# data-flow intent is unchanged (the card reads the derived label, never
# page.status).
# -----------------------------------------------------------------------------
assert_grep_present \
    'sessionStatusLabel' \
    'web/console/src/routes/(console)/live-runtime/+page.svelte' \
    'W10 Live Runtime derives sessionStatusLabel from the strip'
assert_grep_present \
    'sessionStatus=\{sessionStatusLabel\}' \
    'web/console/src/lib/components/live-runtime/active-sessions-panel.svelte' \
    'W10 SessionDetailCard reads sessionStatusLabel, not page.status'

# -----------------------------------------------------------------------------
# W11 — Agents page empty copy explains the synthetic-default posture so
# the count mismatch with Live Runtime ("default agent") is intentional.
# -----------------------------------------------------------------------------
assert_grep_present \
    'synthetic' \
    'web/console/src/routes/(console)/agents/+page.svelte' \
    'W11 Agents page empty copy names the synthetic-default posture'

# -----------------------------------------------------------------------------
# N11 — Overview counter labels carry the "(now)" suffix so the
# point-in-time semantic is explicit.
# -----------------------------------------------------------------------------
assert_grep_present \
    'Tasks Running \(now\)' \
    'web/console/src/routes/(console)/overview/+page.svelte' \
    'N11 Overview Tasks-Running label carries (now) suffix'
assert_grep_present \
    'Background Jobs \(now\)' \
    'web/console/src/routes/(console)/overview/+page.svelte' \
    'N11 Overview Background-Jobs label carries (now) suffix'
assert_grep_present \
    'MCP Connections \(now\)' \
    'web/console/src/routes/(console)/overview/+page.svelte' \
    'N11 Overview MCP-Connections label carries (now) suffix'

# -----------------------------------------------------------------------------
# N12 — Tools right-rail "Active" KPI is relabelled to disambiguate
# (currently-in-flight, not catalog-ever-fired).
# -----------------------------------------------------------------------------
assert_grep_present \
    'In-flight \(now\)' \
    'web/console/src/lib/components/tools/ToolOverviewCard.svelte' \
    'N12 Tools overview Active KPI relabelled to In-flight (now)'

# -----------------------------------------------------------------------------
# N13 — Tools RELIABILITY column has an explicit min-width token so
# tier values like `production-tested` no longer truncate mid-glyph.
# -----------------------------------------------------------------------------
assert_grep_present \
    'size-col-reliability' \
    'web/console/src/lib/tokens.css' \
    'N13 size-col-reliability token is defined'
assert_grep_present \
    'width: .var\(--size-col-reliability\)' \
    'web/console/src/routes/(console)/tools/+page.svelte' \
    'N13 Tools page Reliability column uses the size-col-reliability token'

# -----------------------------------------------------------------------------
# N14 — Live Runtime status-counter strip pillars carry the "(now)"
# suffix so the point-in-time semantic is explicit.
# -----------------------------------------------------------------------------
for label in 'Pending \(now\)' 'Running \(now\)' 'Completed \(now\)' 'Paused \(now\)' 'Failed \(now\)'; do
    assert_grep_present \
        "${label}" \
        'web/console/src/lib/components/live-runtime/status-counter-strip.svelte' \
        "N14 Live Runtime strip carries ${label} label"
done

smoke_summary
