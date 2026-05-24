# Phase 83p — settings-add-runtime-form

## Summary

Phase 83p closes Bug **F1** from the post-83k visual walkthrough:
the Settings page's `<PageState>` wrapper short-circuits the whole
cards loop when `status === 'disconnected'`, which means the
`ConnectedRuntimesCard` (and every other Console-local section) is
hidden behind the same disconnected placeholder it's supposed to help
the operator escape. The "+ Add Runtime" form exists in the codebase
but is structurally unreachable on a freshly-booted Console.

The fix is a tight refactor of `src/routes/(console)/settings/+page.svelte`:
split `SETTINGS_SECTIONS` into two groups by data dependency —
console-local sections render unconditionally, runtime-posture
sections stay wrapped in `<PageState>`. The existing
`SettingsState.load()` docstring already documents this split as the
intended behavior ("the Console-local sections (Connected Runtimes,
Per-Runtime Auth, Appearance, …) do NOT depend on the runtime
posture"). The page template just never honored it.

## RFC anchor

- RFC §5 — Console as Protocol client (the entire connect-to-Runtime
  story).
- RFC §8 — CLI surface (the `harbor console` first-run experience).

## Briefs informing this phase

- brief 12
- brief 11

## Brief findings incorporated

- brief 12 §3 — The Console-local DB is the source of truth for
  operator preferences (saved views, address book, auth profiles,
  PATs). These tables exist on the Console-side regardless of any
  Runtime connection; the Settings UI surfaces over them MUST work
  before a Runtime is attached.
- brief 11 §5 — Settings is the operator's escape hatch. Every other
  page can degrade to "disconnected · attach in Settings"; Settings
  itself must NEVER do that for the sections needed to perform the
  attach.

## Findings I'm departing from (if any)

None.

## Goals

- The "+ Add Runtime" form renders on the Settings page when the
  Console has no Runtime attached.
- Every Console-local section (Connected Runtimes, Per-Runtime Auth,
  API Tokens, Appearance, Time & Locale, Keybindings, Notifications
  Routing) renders in the disconnected state.
- Runtime-posture sections (Runtime Info, Governance Posture, Storage
  Drivers, LLM-Provider Posture, About) stay wrapped in `<PageState>`
  and continue to show the "Not connected" placeholder + Retry button
  in the disconnected / error states.
- The existing settings-page Playwright test
  (`web/console/tests/settings-page.spec.ts::settings page shell
  renders even when disconnected`) gains a sibling assertion that the
  Connected Runtimes form is now reachable.
- A new test asserts the add-runtime form submits + the new row
  appears in the address-book table.

## Non-goals

- **Activating a connection from the address book.** Bug F1 is
  specifically the missing add-form; the "click row → make this the
  active connection" flow is a separate concern (already wired
  through the connection store; covered by other tests).
- **Polishing the empty-state of every disconnected page.** That's
  Bug W1/W2/W3/N4/N5/N8/N9/N10 from the walkthrough — a separate
  phase (83r) handles those.
- **Sidebar nav for Playground.** Bug F2 — separate phase (83q).

## Acceptance criteria

- [ ] `SETTINGS_SECTIONS` (in `src/lib/settings/state.svelte.ts`)
      annotates each entry with a `group: 'console-local' |
      'runtime-posture'` discriminator.
- [ ] `+page.svelte`'s cards loop splits into two: console-local
      sections render unconditionally; runtime-posture sections stay
      inside `<PageState>`.
- [ ] In the disconnected state, the Connected Runtimes section
      renders the add-runtime form + (empty) address-book table.
- [ ] In the disconnected state, the runtime-posture group shows the
      `<PageState>` disconnected placeholder (one card, not
      per-section).
- [ ] The existing Playwright `settings page shell renders even when
      disconnected` test still passes.
- [ ] A new Playwright test
      (`settings page lets operator add a runtime when disconnected`)
      asserts: form is visible without a Runtime attached, submitting
      `(name, http://127.0.0.1:18080)` adds a row to the address-book
      table.
- [ ] `scripts/smoke/phase-83p.sh` asserts the static surface change.

## Files added or changed

- `web/console/src/lib/settings/state.svelte.ts` — `SETTINGS_SECTIONS`
  gains `group` field; export helper `consoleLocalSections()` +
  `runtimePostureSections()`.
- `web/console/src/routes/(console)/settings/+page.svelte` — split
  the cards loop; the `<PageState>` boundary now wraps only the
  runtime-posture group.
- `web/console/tests/settings-page.spec.ts` — extend the disconnected
  test to assert the add-form is reachable; add the form-submission
  test.
- `docs/plans/README.md` — Phase 83p row + flip to Shipped.
- `docs/decisions.md` — D-158.
- `docs/glossary.md` — `Settings two-group layout` entry.
- `docs/plans/phase-83p-settings-add-runtime-form.md` — this plan.
- `scripts/smoke/phase-83p.sh` — static-surface assertions.

## Public API surface

- `SETTINGS_SECTIONS[i].group: 'console-local' | 'runtime-posture'`
  added (additive — existing consumers can ignore the new field).
- Two new exported helpers: `consoleLocalSections()` and
  `runtimePostureSections()` — both return filtered subsets of
  `SETTINGS_SECTIONS`.

## Test plan

- **Unit:** N/A (refactor; the existing state tests cover the
  unchanged `SettingsState.load()` behavior).
- **Integration:** Playwright extension to `settings-page.spec.ts` —
  the disconnected-state add-form happy path.
- **Manual:** `harbor console` zero-config, navigate to `/settings`,
  click `+ Add Runtime`, fill `(harbor-dev, http://127.0.0.1:18080)`,
  Save, observe new row in the table. Verified before commit.

## Smoke script additions

`scripts/smoke/phase-83p.sh` asserts:

- `SETTINGS_SECTIONS` carries the `group` discriminator.
- The +page.svelte template references the two-group split (greps
  for `consoleLocalSections` + `runtimePostureSections`).
- The Playwright test file references the new add-form test.

## Coverage target

- `web/console/src/lib/settings`: existing coverage gates apply
  (svelte-check + Playwright e2e).

## Dependencies

- Phase 73m (Settings page + ConnectedRuntimesCard).
- Phase 73p (Console-local DB + saved-views surfaces).

## Risks / open questions

- **Test fixture relies on a Runtime that's not running.** The
  Playwright e2e uses the fixture's seeded runtime; the new test
  must NOT seed a connection, exercising the actual disconnected
  path. The fixture already supports "seed auth but no connection"
  (line 226 of `settings-page.spec.ts`); the new test reuses it.
- **Persisted address-book pollution.** The Console-local DB is a
  per-browser-profile store; the Playwright fixture uses an isolated
  context per test, so no cross-test contamination.

## Glossary additions

- **Settings two-group layout** — Phase 83p / D-158. Splits the
  Settings page sections by data dependency: console-local sections
  (Connected Runtimes, Per-Runtime Auth, API Tokens, Appearance, Time
  & Locale, Keybindings, Notifications Routing) render
  unconditionally; runtime-posture sections (Runtime Info, Governance
  Posture, Storage Drivers, LLM-Provider Posture, About) wrap in
  `<PageState>`. Closes the F1 bug from the post-83k walkthrough where
  the entire page short-circuited to disconnected, hiding the form an
  operator needs to fix the disconnection.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] svelte-check + Playwright e2e pass
- [ ] Concurrent-reuse — N/A (Console-only)
- [ ] Integration test exists per §17 — Playwright happy path
- [ ] Glossary updated
- [ ] If a brief finding was departed from: N/A
