# Phase 83q — playground-sidebar-nav

## Summary

Closes Bug **F2** + Nit **N1** from the post-83k visual walkthrough.
The Playground page route (`/playground`) exists and renders — but
it is unreachable from the Console's sidebar navigation, and its
breadcrumb shows lowercase `playground` while every other page is
Title Case. Tiny, isolated fix.

## RFC anchor

- RFC §5 — Console as Protocol client (consistency of the nav
  shell).
- RFC §7 — Console feature surface (Playground belongs in it).

## Briefs informing this phase

- brief 11
- brief 12

## Brief findings incorporated

- brief 11 §1 — The Playground is one of the Console's first-class
  surfaces (sandbox for steering / inject-context / replay). It must
  be reachable from the nav.
- brief 12 §3 — Navigation consistency is part of the Console's
  visual contract; one-off lowercase breadcrumbs erode trust.

## Findings I'm departing from (if any)

None.

## Goals

- The Console's sidebar nav includes a "Playground" entry, grouped
  appropriately (likely under EXECUTION, alongside Sessions / Tasks).
- The Playground page's breadcrumb shows "Playground" (Title Case),
  matching every other page's pattern.
- Existing Playground tests continue to pass.

## Non-goals

- Filling the Playground's empty-state shell with skeleton content.
  That's a separate item (the post-walkthrough notes flagged the
  page is mostly blank when disconnected; 83r covers the
  disconnected-state hygiene pass).

## Acceptance criteria

- [ ] Sidebar Svelte component renders a "Playground" entry that
      links to `/playground`.
- [ ] The Playground page header / breadcrumb renders "Playground"
      (capital P).
- [ ] An existing Playground test asserts the nav entry is present
      (extend the `harness.spec.ts` nav assertion).
- [ ] No regression in other pages' nav behavior.

## Files added or changed

- `web/console/src/lib/components/console/` (the sidebar/nav
  component — the agent identifies the exact file).
- `web/console/src/routes/(console)/playground/+page.svelte` (or
  its layout file — wherever the breadcrumb is set).
- `web/console/tests/harness.spec.ts` — extend the nav-entries
  assertion if one exists.
- `docs/plans/README.md` — Phase 83q row.
- `docs/decisions.md` — D-159.
- `docs/plans/phase-83q-playground-sidebar-nav.md` — this plan.
- `scripts/smoke/phase-83q.sh` — static-surface assertions.

## Public API surface

None. Pure UI fix.

## Test plan

- **Unit:** N/A.
- **Integration:** extend the existing nav test in
  `web/console/tests/harness.spec.ts` (or the per-page spec where
  the nav is asserted) to include Playground.
- **Manual:** boot `harbor console`, see "Playground" in the
  sidebar, click it, see "Playground" in the breadcrumb (capital P).

## Smoke script additions

`scripts/smoke/phase-83q.sh` asserts:

- The sidebar component file references "Playground".
- The playground route renders "Playground" (capital P).

## Coverage target

- N/A — purely additive UI fix.

## Dependencies

- Phase 73n (Playground page).

## Risks / open questions

- The agent must identify the exact sidebar/nav source file
  (likely `web/console/src/lib/components/console/Sidebar.svelte` or
  similar — confirm via `grep -rln 'Background Jobs' web/console/src/`).

## Glossary additions

None.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] svelte-check + Playwright e2e pass
- [ ] Concurrent-reuse — N/A
- [ ] Integration test exists per §17 — extension of existing nav
      assertion
- [ ] Glossary updated — N/A
- [ ] If a brief finding was departed from: N/A
