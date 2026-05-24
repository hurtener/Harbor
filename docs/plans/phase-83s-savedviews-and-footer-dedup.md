# Phase 83s — savedviews-and-footer-dedup

## Summary

Closes Walkthrough Nits **N2** + **N7** from the post-83k
walkthrough. Pure copy + layout dedup.

- **N2.** Most pages stack TWO "Disconnected · no Runtime attached"
  indicators — one in the bottom of the main column AND one fixed at
  the bottom of the viewport. The viewport-fixed `ConnectionFooter`
  is the canonical surface; the per-page inline one is duplicate noise.
- **N7.** The "Saved views" label drifts across pages — 8 different
  phrasings: "Save current as…" / "Save view as…" / "Save view" /
  "Save snapshot" / "Save filter" / "Save". The concept is the same;
  the label drift erodes consistency. Settle on one verb (Save view)
  AND one input placeholder ("Save current as…").

## RFC anchor

- RFC §5 — Console as Protocol client (visual consistency).

## Briefs informing this phase

- brief 12

## Brief findings incorporated

- brief 12 §3 — Design tokens + label consistency are part of the
  Console's visual contract. Same-purpose labels across pages must
  agree (a "save" gesture means the same thing everywhere).

## Findings I'm departing from (if any)

None.

## Goals

- Settle on ONE saved-views verb + ONE input-placeholder copy
  across every page that surfaces saved views. Suggested:
  - Button label: **"Save view"**
  - Input placeholder: **"Save current as…"**
- Remove every per-page inline "Disconnected · no Runtime attached"
  indicator that duplicates the viewport-fixed `ConnectionFooter`.
  Pages keep the fixed footer; the inline copy goes.

## Non-goals

- Disconnected-state hygiene at the filter / action / card level —
  that's 83r.
- Sidebar nav fixes — that's 83q.
- A new component for saved-views — keep the existing
  `SavedViewChips` + filter-bar Save button intact; only the LABELS
  change.

## Acceptance criteria

- [ ] Every page that surfaces saved views uses "Save view" as the
      button label.
- [ ] Every page that surfaces saved views uses "Save current as…"
      as the input placeholder.
- [ ] No page renders an inline "Disconnected · no Runtime attached"
      indicator outside the fixed `ConnectionFooter`.
- [ ] `scripts/smoke/phase-83s.sh` asserts the surface changes.

## Files added or changed

The agent will touch one or more of:

- `web/console/src/lib/components/ui/SavedViewChips.svelte` (or its
  parents) — the central place to change the placeholder copy.
- Per-page Svelte files — to remove the duplicate inline disconnected
  footer.
- `docs/plans/README.md` — Phase 83s row.
- `docs/decisions.md` — D-161.
- `docs/glossary.md` — none needed.
- `docs/plans/phase-83s-savedviews-and-footer-dedup.md` — this plan.
- `scripts/smoke/phase-83s.sh` — static-surface assertions.

## Public API surface

None.

## Test plan

- **Unit:** N/A.
- **Integration:** Playwright tests assert the consistent saved-views
  label + that no inline "Disconnected" surface remains outside the
  ConnectionFooter.
- **Manual:** boot `harbor console`, walk every page, verify each
  saved-views control reads "Save view" / "Save current as…" + only
  one disconnected indicator per page.

## Smoke script additions

`scripts/smoke/phase-83s.sh` asserts:

- "Save view" appears as the button label on the relevant component(s).
- No per-page Svelte file contains an inline "Disconnected" text
  outside the ConnectionFooter component.

## Coverage target

- Existing Console coverage gates.

## Dependencies

- Phase 73m, 73p (Console foundation).

## Risks / open questions

- **Conflicts with 83r.** Both phases touch per-page Svelte files.
  Scope discipline: 83r touches disconnected-state predicates +
  buttons + cards; 83s touches labels + inline disconnected
  indicators. Coordinator integrates.

## Glossary additions

None.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] svelte-check + Playwright e2e pass
- [ ] Concurrent-reuse — N/A
- [ ] Integration test exists per §17 — Playwright extension(s)
- [ ] Glossary updated — N/A
- [ ] If a brief finding was departed from: N/A
