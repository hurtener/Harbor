# Phase 108f — Console Settings page (calm sub-nav + single-section model)

> Page-polish phase against `docs/design/console/CONVENTIONS.md` +
> `docs/design/console/PAGE-POLISH-PROCEDURE.md`. The current Settings page
> (Phase 73m / D-129) is over-engineered — it runs three navigation models at
> once (a sub-nav rail AND scroll-to-anchor AND 6-per-page pagination), plus a
> top FilterBar, saved-view chips, a "Bookmark section" button, and a detail
> rail. This phase strips it to the calm model the page's own spec §4 already
> prescribes: a sub-nav rail and a right pane that shows ONLY the active
> section.

## Summary

Simplify the Console Settings page to a "sub-nav rail + one section at a time"
layout. The left rail lists the 12 sections (lightly grouped: Console-local,
then a Runtime sub-heading for the read-only posture sections); the right pane
renders ONLY the active section as a carded `<section class="panel card">`
mirroring the Overview page's (108c) vocabulary. All cruft — FilterBar,
SavedViewChips, the Bookmark-section button, the detail rail, pagination, the
duplicate runtimes DataTable, and scroll-to-anchor — is removed. The D-158
console-local / runtime-posture split is preserved per active section so the
disconnected attach path still works.

## RFC anchor

- RFC §7
- RFC §7.1

## Briefs informing this phase

- brief 11
- brief 12

## Brief findings incorporated

- brief 11 §"Settings view": the Settings surface is a calm per-operator +
  per-runtime configuration page — a left section list with one section's
  content shown at a time, not a dense multi-pane dashboard. This phase adopts
  that calm model and removes the 73m over-build that drifted from it.
- brief 12 §"auth-storage threat model": per-runtime auth + tokens persist
  encrypted in the Console DB (D-061); the page never shadows runtime entities.
  Preserved unchanged — only the page composition changes, not the data layer.

## Findings I'm departing from (if any)

None. This phase RE-aligns the page to brief 11 §"Settings view" and
page-settings.md §4 (the sub-nav + single-section model), reversing the 73m
composition that departed from them.

## Goals

- Replace the 73m three-nav-models composition with a single calm model:
  sub-nav rail on the left, the active section (and only it) on the right.
- Adopt the Overview (108c) carded vocabulary (`.panel.card` + `.panel-title`),
  tokens only, Svelte 5 runes (D-092).
- Preserve the D-158 console-local / runtime-posture split per active section
  so the disconnected attach path (the operator's only path to attach a
  runtime) keeps working.
- Keep every section card component, the state module, and `console_db` unchanged.

## Non-goals

- No change to the section CARD components, `state.svelte.ts`, or
  `console_db.svelte.ts`.
- No new Protocol method, no Protocol surface change (the page stays a pure
  consumer of the 72f/72g posture methods + the 72h Console DB).
- No deletion of `$lib/settings/saved_views.svelte.ts` (left in place; merely
  no longer imported by the page).

## Acceptance criteria

- [ ] The page is a two-pane flex: `<SubNavRail>` + a `.section-pane` that
  renders ONLY the active section (default `connected-runtimes`).
- [ ] Each active section is a `<section class="panel card">` with an
  `<h2 class="panel-title" data-testid="settings-active-section">` reading the
  section label.
- [ ] A console-local active section renders directly inside a
  `data-testid="settings-cards-console-local"` wrapper; a runtime-posture
  active section renders inside `<PageState>` inside a
  `data-testid="settings-cards-runtime-posture"` wrapper (D-158).
- [ ] The page keeps `visibleConsoleLocal` / `visibleRuntimePosture` derived
  subsets and the `settings-page`, `settings-subnav` / `settings-subnav-<id>`,
  and `settings-active-section` testids.
- [ ] FilterBar, SavedViewChips, the Bookmark-section button +
  `settings-save-view` testid, the detail rail, pagination, the duplicate
  runtimes DataTable, and scroll-to-anchor are all removed.
- [ ] `npm run check` 0/0, `npm run lint` clean, `npm run test` green.
- [ ] `scripts/smoke/phase-108f.sh` 0 FAIL; phase-83p / phase-83u / phase-105
  stay green.

## Files added or changed

- `web/console/src/routes/(console)/settings/+page.svelte` — rewritten to the
  single-section model.
- `web/console/src/lib/components/settings/SubNavRail.svelte` — tidied; light
  grouping (Console-local, divider + "Runtime" sub-heading, posture sections).
- `web/console/tests/settings-page.spec.ts` — rebuilt for the single-section model.
- `scripts/smoke/phase-108f.sh` — new static-only guard.
- `docs/plans/phase-108f-console-settings-page.md` — this plan.
- `docs/decisions.md` — D-178.
- `docs/design/console/page-settings.md` — §13 reframe note.

## Public API surface

N/A — Console-only page-polish phase. No Go public API, no new Protocol method.

## Console consistency (CONVENTIONS.md §9 + PAGE-POLISH-PROCEDURE.md)

- **Route group + shell.** Stays under `(console)/settings/+page.svelte`,
  served at `/settings`, rendered inside the one app shell. The
  ConnectionFooter is rendered once by the shell, never per-page.
- **`<PageState>` four-state contract.** The runtime-posture sections route
  through `<PageState>` (loading / ready / empty / error + the disconnected
  branch); console-local sections render directly (D-158).
- **Shared `ui/` inventory + tokens.** Carded `.panel.card` + `.panel-title`
  vocabulary copied from the Overview page (108c); design tokens only — no raw
  color / spacing / type-scale literals. The rail uses `--size-nav`.
- **HarborClient + connection.ts.** All async state flows through the
  `SettingsState` / `SettingsDBController` modules, which use `HarborClient` +
  `connection.ts` — no hand-rolled `fetch` in the page.
- **Viewport discipline (PAGE-POLISH-PROCEDURE).** The left rail is fixed
  (sticky); the right pane scrolls internally if a section is long — the chrome
  never full-page-scrolls.

## Test plan

- **Unit:** existing settings vitest suites (`add-runtime-form.test.ts`,
  `console_db.spec.ts`) stay green unchanged.
- **Integration:** `web/console/tests/settings-page.spec.ts` (Playwright e2e)
  rebuilt for the single-section model — hydration, sub-nav → active-section,
  the default-section add-runtime round-trip, rotate-token gating, mock-mode
  banner, disconnected shell, the D-158 disconnected attach path, and the 83u
  disconnected-boot localStorage write.
- **Conformance:** N/A — no driver/interface added.
- **Concurrency / leak:** N/A — no reusable Go artifact built.

## Smoke script additions

- `scripts/smoke/phase-108f.sh` (static-only): asserts the page no longer
  imports/uses FilterBar / SavedViewChips / Pagination / DetailRail, drops the
  `settings-save-view` testid, keeps `settings-page` / `settings-subnav` / the
  single `settings-active-section` heading / `settings-cards-console-local` /
  `settings-cards-runtime-posture` / `visibleConsoleLocal` /
  `visibleRuntimePosture` / `AttachToLocalCard`.

## Coverage target

`web/console` (front-end): the rebuilt Playwright spec + the unchanged vitest
suites must pass; no Go coverage delta (no Go code touched).

## Dependencies

- Phase 73m (the Settings page this phase simplifies)
- Phase 83p / D-158 (the console-local / runtime-posture split, preserved)
- Phase 105 (first-attach — AttachToLocalCard, preserved)
- Phase 108c (the Overview carded vocabulary this phase copies)

## Risks / open questions

- The sub-nav rail now omits scroll-to-anchor; selecting a section swaps the
  right pane instantly. Low risk — the page's own spec §4 prescribes exactly
  this.
- `$lib/settings/saved_views.svelte.ts` becomes unused by the page; left in
  place to avoid breaking other refs/tests. A future cleanup phase may remove it.

## Glossary additions

None.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A, no isolation paths touched.
- [ ] If this phase builds a reusable artifact: concurrent-reuse test passes — N/A, no reusable Go artifact built (Console-only page-polish).
- [ ] If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists — covered by the rebuilt Playwright e2e (front-end seam); no Go seam touched.
- [ ] If new vocabulary: glossary updated — N/A.
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — no departure; D-178 records the re-alignment.
