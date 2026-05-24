# Phase 83x — real-data-layout-bugs

## Summary

Closes Walkthrough-round-2 Bugs **W4** through **W11** + Nits
**N11**, **N12**, **N13**, **N14**. Per-page polish band — every
item is a layout / data-projection / wire-surface mismatch surfaced
by running the YouTube agent end-to-end. None is a showstopper
individually; together they form the "you can see the runtime is
working but every page has a paper cut" backdrop.

## RFC anchor

- RFC §5 — Console as Protocol client.
- RFC §6.4 — Tool catalog (W7 — Tasks kanban).
- RFC §6.6 — Memory (W4).
- RFC §6.10 — Artifacts (W5, W6).
- RFC §6.13 — Events bus (W9).

## Briefs informing this phase

- brief 11
- brief 06

## Brief findings incorporated

- brief 11 §2 — Every page is a runtime lens; the projection from
  runtime data to the UI must be lossless. W4 / W5 / W6 / W7 are
  projection losses.
- brief 06 §4 — Console pages must reflect the actual runtime
  state. W8 / W9 / W10 / W11 are state-source mismatches.

## Findings I'm departing from (if any)

None.

## Goals

### W4 — Memory page MEMORY KEY column wraps text vertically

The MEMORY KEY column renders each character on its own line for
real keys. Fix: `text-overflow: ellipsis` + tooltip on hover, OR
widen the column + monospace + safe break-points.

### W5 — Artifacts page right-rail card overlaps the table

The "SELECTED ARTIFACT" right-rail visually overlaps the SOURCE /
TAGS columns. Fix: grid / flex layout that reserves the right-rail
column properly.

### W6 — Artifacts `created_at` is the Go zero value

The artifact row reads CREATED `0001-01-01T00:00:00Z`. Whatever
populates the artifact upload (likely
`cmd/harbor/cmd_dev_executor.go::projectForLLM` where the heavy-
output promotion happens) needs to set `CreatedAt = time.Now()`.
The wire-projection layer in `internal/artifacts/protocol/` may
need to populate it too.

### W7 — Tasks page kanban view has no Complete column

The kanban shows Pending / Running / Paused / Failed columns; the
completed task is counted in the right-rail SUMMARY (Complete: 1)
but invisible in the kanban. Fix: add a Complete column to the
kanban (preferred), OR change the default view to "List view",
OR show a "1 completed (view in list mode)" hint.

### W8 — Sessions page shows zero rows despite a live session

The Sessions list reads "No sessions match these filters" even
though the dev/dev/dev session is the active connection scope +
just ran a 13.7s task. Probably the dev pseudo-session is implicit
(auto-created from the JWT identity claim) but not explicitly
written to the SessionRegistry on first task spawn. Fix: ensure
the SessionRegistry has a row for the active identity scope when
the first task spawns under it.

### W9 — Events page shows zero rows despite live events firing

The Events page reads zero events with `Last 1 h` filter selected
even though the bus fired many during the task run. Likely root
cause: the Events page calls `events.search` which requires the
`durable` driver, not `inmem`. Fix: either (a) document the
requirement in the empty-state copy ("Events list requires the
`durable` event driver — see docs/CONFIG.md#events.driver"), or
(b) extend the inmem driver to expose its replay buffer to the
search/list surface.

### W10 — Live Runtime "default agent" status reads "error"

The session detail right-rail shows Status: **error** even though
the task completed successfully (and `Last error: none`). Fix:
status derivation should read the most-recent TASK status
(`Complete`) instead of inferring from history.

### W11 — Agents page reports zero agents while Live Runtime says "default agent"

Same Runtime, two different agent counts. The "default agent" is a
synthetic label; the Agents page reads from the AgentRegistry which
has zero registered rows. Fix: document the synthetic-default
posture explicitly (Agents page: "0 registered agents · the
runtime runs a default agent for the dev token's identity scope —
see docs/CONFIG.md for registering named agents").

### N11 — Overview metrics window doesn't include just-completed task

The Overview right-rail counters all read 0 after a completed task.
Either the metrics are "currently active" (the current behavior;
needs labelling) or the operator's mental model expects "completed
in window". Fix: relabel pillar to "Sessions active (now)" /
"Tasks running (now)" so the semantics are explicit.

### N12 — Tools page "Active" KPI is ambiguous

Tools page right-rail says `Active 0` after the tool was invoked.
Means "currently in-flight". Fix: relabel "Active (in-flight)" or
"In-flight" so the semantics are explicit.

### N13 — Tools page table truncates the RELIABILITY column

Minor h-scroll / column-width tuning needed.

### N14 — Live Runtime status pills suggest task-state but session is across runs

Pending 0 / Running 0 / Completed 0 / Paused 0 / Failed 0 — all
zero after a completed task. Either label "Active by status
(current)" or include historical counts.

## Non-goals

- New showstopper fixes (F3 / F4 / F5 / F6 — separate phases).
- Round-3 re-walkthrough (only after 83u / 83v / 83w / 83x land).

## Acceptance criteria

- [ ] W4 — Memory page keys render readably (no vertical char-per-
      line wrap).
- [ ] W5 — Artifacts page right-rail does not overlap the table.
- [ ] W6 — Artifacts `created_at` populated with `time.Now()` at
      promotion time + projected on the wire.
- [ ] W7 — Tasks kanban shows the completed task (either via new
      column or by changing the default view).
- [ ] W8 — Sessions page shows the active dev session.
- [ ] W9 — Events page either lists events OR explicitly documents
      the `events.driver: durable` requirement in the empty state.
- [ ] W10 — Live Runtime session status reads "complete" after
      task completion.
- [ ] W11 — Agents page empty state explains the synthetic-default
      posture (or the default agent appears as a registered row).
- [ ] N11 — Overview metric labels carry the "(now)" suffix.
- [ ] N12 — Tools "Active" KPI relabelled.
- [ ] N13 — RELIABILITY column wide enough.
- [ ] N14 — Live Runtime pillar labels carry the "(now)" suffix.
- [ ] `scripts/smoke/phase-83x.sh` asserts the static surface.

## Files added or changed

- `web/console/src/routes/(console)/memory/+page.svelte` or the
  memory-row component — W4.
- `web/console/src/routes/(console)/artifacts/+page.svelte` or
  layout — W5.
- `internal/artifacts/...` AND/OR
  `cmd/harbor/cmd_dev_executor.go` — W6.
- `web/console/src/routes/(console)/tasks/+page.svelte` — W7.
- `internal/tasks/...` (session-row create on first task spawn) —
  W8.
- `web/console/src/routes/(console)/events/+page.svelte` (empty-
  state copy) — W9.
- `internal/runtime/runs/...` (session status derivation) — W10.
- `web/console/src/routes/(console)/agents/+page.svelte` (empty-
  state copy) — W11.
- `web/console/src/routes/(console)/overview/+page.svelte` — N11.
- `web/console/src/routes/(console)/tools/+page.svelte` — N12 /
  N13.
- `web/console/src/routes/(console)/live-runtime/+page.svelte` —
  N14.
- `docs/plans/README.md` — Phase 83x row.
- `docs/decisions.md` — D-165.
- `docs/plans/phase-83x-real-data-layout-bugs.md` — this plan.
- `scripts/smoke/phase-83x.sh`.

## Public API surface

- W6 — `prototypes.Artifact` already has `CreatedAt`; the change
  is to populate it, not to add it.
- W8 — depends on whether SessionRegistry already has an
  auto-create path or if it's a new gesture.

## Test plan

- Per-page smoke greps for the surface change.
- Playwright extensions where straightforward (W7 kanban Complete
  column visibility, W9 empty-state copy).
- Manual re-walk against the YouTube agent after 83x lands to
  verify each item.

## Smoke script additions

`scripts/smoke/phase-83x.sh` asserts each of W4-W11 + N11-N14 has
its target change in source.

## Coverage target

- Per-page; existing coverage gates.

## Dependencies

- Phase 73m, 73p (Console foundation).
- Phase 83i (memory writeback — W4's data exists because of it).
- Phase 83m item 7 (`task.tool_count` — Tasks kanban gets the
  counter from this).

## Risks / open questions

- **W6 spans Go + Console.** The agent should patch both sides
  (Go-side time.Now() + wire-projection layer in
  internal/artifacts/protocol/).
- **W8 + W10 are tasks-subsystem changes.** Larger scope than the
  others; may surface additional latent bugs (§17.6 — fix them
  in-PR if they're small).
- **W9 could be either copy or storage-driver work.** Default to
  the copy fix (low-risk); the storage-driver extension would be
  its own phase.

## Glossary additions

None — pure polish.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] svelte-check + Playwright e2e pass
- [ ] Concurrent-reuse — N/A
- [ ] Integration test exists per §17 — Playwright extensions
- [ ] Glossary updated — N/A
- [ ] If a brief finding was departed from: N/A
