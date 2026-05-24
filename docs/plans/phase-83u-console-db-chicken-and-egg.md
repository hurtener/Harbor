# Phase 83u — console-db-chicken-and-egg

## Summary

Closes Bug **F3** from the round-2 walkthrough. Phase 83p made the
Connected Runtimes add-form REACHABLE in the disconnected state but
its submit path is structurally broken: `console_db.svelte.ts::addRuntime`
calls `this.#db.runtimes.upsert(...)` on a DB that requires a
`RuntimeConnection` to derive its per-operator encryption key.
Operator without a Runtime → no connection → DB stays closed →
addRuntime throws "Console DB not open — attach to a Runtime first".
Operator can't attach a Runtime through the UI without first
attaching a Runtime through the UI. Closed loop.

The fix splits the add-form's two effects:

1. **Set the active connection** (write to `localStorage`'s
   `harbor.runtime.*` keys). This is what makes the Console talk
   to a Runtime. Has no DB dependency.
2. **Persist to the address book** (Console DB `runtime_registry`
   table). Convenience for switching between multiple Runtimes
   later. Lazy: only writes after a successful first attach unlocks
   the DB.

83u rewires the form so step (1) always works (no DB), step (2) is
deferred (best-effort, fires after the next page reload when the
attached connection unlocks the DB). The address book then catches
up with the rows the operator already added.

## RFC anchor

- RFC §5 — Console as Protocol client.
- RFC §7 — Console feature surface (Settings page).

## Briefs informing this phase

- brief 11
- brief 12

## Brief findings incorporated

- brief 11 §5 — Settings is the operator's escape hatch. The
  Connected Runtimes form is the single most important control in
  the entire Console; it MUST work pre-attach.
- brief 12 §3 — Console-local storage layering: `localStorage`
  holds the active connection triple; the Console DB is the cross-
  attach address book + encrypted secret store. 83u clarifies the
  separation by making the active-connection write happen at the
  primary layer (localStorage), not the convenience layer (DB).

## Findings I'm departing from (if any)

None.

## Goals

- The Connected Runtimes add-form on the Settings page writes the
  active connection to `localStorage` first (the operator's primary
  intent — "make the Console talk to this Runtime") and only THEN
  attempts to persist to the Console DB.
- A DB-write failure does NOT block the connection — surfaces a
  non-fatal warning in the UI ("Saved as the active runtime;
  address-book persistence will happen after first page reload").
- After the next page reload + successful attach, the address book
  auto-imports the active-connection entry if it's not already
  present. (Background sync.)
- A unit test asserts: with no Runtime attached, calling
  `addRuntime("name", "url")` updates localStorage to point at
  the new URL + (when DB is open) writes to the address book.
- A Playwright test asserts: zero-config `harbor console`, click +
  Add Runtime, fill form, Add → page reloads → connection footer
  shows "● Connected" + address book contains the new entry.

## Non-goals

- **Encrypting the address-book entries.** The Runtime URL itself
  isn't a secret (operators see it in `harbor dev` logs); the JWT
  token already lives in localStorage. No new encryption needed.
- **Adding a "test connection" button to the form before save.**
  Out of scope; the address-book entry doesn't need to be reachable
  to be saved.
- **Migration of pre-83u address-book entries.** Pre-83u operators
  never reached this code path (the form errored on first use), so
  there's nothing to migrate.

## Acceptance criteria

- [ ] `web/console/src/lib/connection.ts` gains an
      `attachConnection(baseURL, options)` helper that writes the
      `harbor.runtime.*` localStorage keys. Existing
      `resolveConnection()` continues to read from the same keys.
- [ ] `web/console/src/lib/settings/console_db.svelte.ts::addRuntime`
      calls `attachConnection()` first, then attempts the DB write
      (catching failures into a non-fatal warning state).
- [ ] The form's onsubmit handler shows the warning + reloads the
      page on success (so the new connection takes effect AND the
      DB opens on the reloaded page, enabling the address-book
      catch-up).
- [ ] An "address-book catch-up" routine runs on Console DB load:
      if the active connection is not already in the address book,
      it's inserted with `is_default: 1`.
- [ ] Playwright test: from a clean Console boot (no prior
      connection in localStorage), the add-form happy-path round-
      trips through to a connected state.
- [ ] `scripts/smoke/phase-83u.sh` asserts the static surface.

## Files added or changed

- `web/console/src/lib/connection.ts` — `attachConnection()`
  helper.
- `web/console/src/lib/settings/console_db.svelte.ts` —
  `addRuntime` rewires through `attachConnection()` first.
- `web/console/src/lib/components/settings/ConnectedRuntimesCard.svelte`
  — minor copy / reload-on-submit handling.
- `web/console/tests/settings-page.spec.ts` — extend the
  "add a runtime when disconnected" test from 83p to actually
  follow through (was prevented by F3).
- `docs/plans/README.md` — Phase 83u row.
- `docs/decisions.md` — D-163.
- `docs/plans/phase-83u-console-db-chicken-and-egg.md` — this plan.
- `scripts/smoke/phase-83u.sh`.

## Public API surface

- `connection.ts` exports `attachConnection(baseURL, opts)` —
  callable from anywhere a future "switch to this runtime" gesture
  needs the same write semantics.

## Test plan

- **Unit:** `attachConnection()` round-trip through localStorage
  (the same testing pattern existing connection-store tests use).
- **Integration:** Playwright end-to-end (clean → form → reload →
  connected).
- **Manual:** boot `harbor console`, no prior localStorage state,
  navigate to Settings, fill form, Add, watch the page reload + the
  connection footer turn green.

## Smoke script additions

- `connection.ts` exports `attachConnection`.
- `addRuntime` references `attachConnection`.
- The Playwright test asserts the post-add reload + connected
  state.

## Coverage target

- `web/console/src/lib/connection.ts` — bump to cover the new
  helper.

## Dependencies

- Phase 73m, 73p, 83p (Settings two-group layout).

## Risks / open questions

- **Page reload UX.** A connection change requires a reload (every
  page subscribes to the connection on mount). 83u's form
  triggers it explicitly; a future enhancement could make the
  connection store reactive enough to skip the reload, but V1
  ships the reload.
- **Race on DB catch-up.** Two browser tabs both pre-83u would
  collide on the address-book write. Mitigated by the DB's per-
  operator transactional upsert — last-write-wins, no corruption.

## Glossary additions

- **`attachConnection()` helper** — Phase 83u. The single Console-
  side write path for the active Runtime connection. Updates
  localStorage `harbor.runtime.*` keys; bypasses the Console DB
  entirely so the operator's first-attach gesture works without
  the chicken-and-egg.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] svelte-check + Playwright e2e pass
- [ ] Concurrent-reuse — N/A
- [ ] Integration test exists per §17 — Playwright end-to-end
- [ ] Glossary updated
- [ ] If a brief finding was departed from: N/A
