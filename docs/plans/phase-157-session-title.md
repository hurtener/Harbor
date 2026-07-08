# Phase 157 — Session title: record field + `sessions.set_title` + Console rename

## Summary

Sessions are displayed by raw id everywhere (the Sessions page renders `shortSessionID`, the Playground switcher renders the bare `session_id` in `<option>` text) because the session record carries no human-readable name and no verb exists to set one. This phase adds a `Title` + `TitleSource` pair to the canonical session record (additive JSON round-trip, zero migration), a `sessions.set_title` Protocol method (manual-only writes, identity-disciplined like `sessions.delete`), a content-free `session.title_changed` canonical event, and the Console consumers that make the primitive load-bearing in the same wave (§13): title display + inline rename on the Sessions page, and title-or-id display + active-session rename in the Playground switcher. `TitleSource` lands now (`unset | auto | manual`) so Phase 158's auto-naming needs no schema reshuffle and manual-wins is structurally enforceable from day one.

## RFC anchor

- RFC §6.9
- RFC §5.2
- RFC §6.13
- RFC §7

## Briefs informing this phase

- brief 05
- brief 06
- brief 11

## Brief findings incorporated

- brief 05 §5: "Session updates written as audit events keyed by a `session:{id}` string" is the compatibility trick Harbor rejected — "sessions are first-class with their own table". A session title is session-entity metadata and lives ON the first-class record, never in a side-channel store (this is also the D-061 shadow-store rule: the Console must not hold the only copy of a runtime entity's attribute).
- brief 05 §6: "Cross-tenant isolation. Storing an artifact under tenant A and attempting to read under tenant B fails. Same for tasks, sessions, memory, trajectories." — the set-title write gets the same cross-identity refusal tests the erasure surface carries.
- brief 06 §5: "avoid logging large payloads; prefer artifacts/resources and log references" + the metrics-cardinality rule against free-form input — extended here to the event bus: the title is user-derived free text and NEVER rides an event payload (SafePayloads forbid raw user input; the redactor only catches secret-shaped keys, so a non-safe payload would leak it too). `session.title_changed` carries identity scope + source only; consumers refetch.

## Findings I'm departing from (if any)

- None.

## Goals

- A session carries an optional human-readable `Title` (+ `TitleSource`) on its canonical record, persisted through the existing `session.lifecycle` StateStore round-trip, projected on `SessionRow`, and erased with the session.
- An authenticated caller can set, change, and clear a title for a session belonging to their verified `(tenant, user)` via `sessions.set_title`. The wire verb ALWAYS writes `TitleSource = manual` — `auto` is not expressible over the wire (Phase 158's internal write path is the only producer of `auto`), so a manual title can never be forged into an auto-overwritable one.
- The Console shows titles wherever sessions are listed: Sessions page (truncated title with full-title+id tooltip, id fallback when unset) and the Playground session switcher (`title || session_id`). Both ship a rename affordance — this is the §13 in-repo primitive consumer.
- Rename observability without content leakage: `session.title_changed` (SafePayload: identity scope, session id, source — never the title string).

## Non-goals

- No auto-naming, no counters, no naming policy — Phase 158.
- No admin/fleet cross-tenant rename widening (a D-284-style aggregating write is a separate decision if the need emerges; v1.12 scope is the owning `(tenant, user)` only).
- No title search/filter on `sessions.list` (additive later if a consumer asks).
- No backfill of titles for existing sessions.
- No title on the `session.erased` record-of-fact or any other event payload (content-free contract, D-286 posture).

## Acceptance criteria

- [ ] `internal/sessions.Session` gains `Title string` + `TitleSource TitleSource` (`""`=unset, `"auto"`, `"manual"`); old persisted records load with both zero-valued (additive JSON, proven by a round-trip test against a pre-change fixture blob).
- [ ] `SessionRegistry.SetTitle(ctx, id, ident, title)` — manual semantics: trims whitespace; rejects titles containing newline/control characters or exceeding `MaxSessionTitleLen` (200 runes) with typed `ErrInvalidTitle` (NO silent clamp, §13); empty-after-trim clears Title AND resets TitleSource to unset; unknown id → `ErrSessionNotFound`; stored-identity `(tenant, user)` mismatch → `ErrIdentityMismatch`. Renaming a CLOSED session is allowed (metadata on a historical conversation); renaming an ERASED session is `not_found`.
- [ ] Authorization scope: the target session must belong to the caller's verified `(tenant, user)` — the same scope `sessions.list` already reads at. The body `Identity`'s tenant/user MUST equal the verified identity (mirroring `assertSessionsIdentity` in the delete path); the target `session_id` rides a dedicated request field and MAY differ from the caller's own session (renaming a sibling session from the Sessions page is the motivating consumer). Cross-user / cross-tenant → `identity_required` / `not_found`; no elevation knob, no admin widening (D-288).
- [ ] `sessions.set_title` Protocol method: `MethodSessionsSetTitle` registered (method set + `canonicalSessionsMethods` + conformance); stream handler branch beside list/inspect/delete; oversize/invalid title → `CodeInvalidRequest` 400; response returns `{session_id, title, title_source}`.
- [ ] `SessionRow` gains `title` + `title_source` (additive, `omitempty`); `projectRow` copies them; `sessions.list` + `sessions.inspect` surface them.
- [ ] Canonical `session.title_changed` event (SafePayload) published on every successful set/clear, carrying identity scope + session id + source ONLY — a test asserts the payload contains no title content (grep-the-marshaled-bytes assertion).
- [ ] Erasure: a titled session's `sessions.delete` leaves no title anywhere (rides the existing `DeleteScope` clear — asserted by extending an existing erasure test); the Phase 155 fault-injection + conformance suites stay green untouched.
- [ ] Console Sessions page: Session column renders the truncated title when set (tooltip = full title + id; id fallback unchanged when unset); an inline rename affordance calls the typed client's `setTitle`, refreshes the row, and surfaces the 400 on invalid input. Tokens-only styling, `<PageState>` contract untouched (D-121).
- [ ] Console Playground: switcher options render `title || session_id`; a rename control on the ACTIVE session calls `setTitle`; the existing live event subscription refreshes the session list on `session.title_changed`.
- [ ] Full lockstep in the same PR: `make protocol-ts-gen` (manifest + `sessions.ts` + `client.ts` mirror), `make protocol-docs-gen`, `singlesource.CanonicalWireTypes`, the three generator `typeindex.go` registrations, `methods_test.go`. `ProtocolVersion` unbumped (additive).
- [ ] `scripts/smoke/phase-157.sh` OK ≥ 3, FAIL = 0 (set→inspect round-trip, clear, oversize-400).

## Files added or changed

- `internal/sessions/sessions.go` — `Title`/`TitleSource` fields, `TitleSource` type + constants, `MaxSessionTitleLen`, `ErrInvalidTitle`, `SetTitle` on the `SessionRegistry` interface.
- `internal/sessions/registry.go` — `SetTitle` (load → verify identity → validate → mutate → `save()` → publish), following the `Touch`/`Close` shape.
- `internal/sessions/events.go` — `EventTypeSessionTitleChanged` + `SessionTitleChangedPayload` (SafeSealed; identity scope + session id + source only).
- `internal/sessions/protocol/lister_projector.go` — `projectRow` copies title fields; the Service gains the set-title op + error mapping (`ErrInvalidTitle` → invalid-request class).
- `internal/protocol/types/sessions.go` — `SessionRow.Title`/`TitleSource`, `SessionsSetTitleRequest{Identity, SessionID, Title}` / `SessionsSetTitleResponse`.
- `internal/protocol/methods/methods.go` (+ `methods_test.go`) — `MethodSessionsSetTitle`.
- `internal/protocol/singlesource/singlesource.go`; `cmd/harbor-protocol-ts-lockstep/typeindex.go`; `cmd/harbor-gen-protocol-docs/typeindex.go` (+ `methods.go` join row); `cmd/harbor-protocol-ts-types/typeindex.go`.
- `internal/protocol/transports/stream/sessions_handler.go` — `serveSetTitle` branch + identity assertion + status mapping.
- `internal/protocol/conformance/conformance.go` — method coverage row.
- `web/console/src/lib/protocol/sessions.ts`, `client.ts`, `wire-manifest.gen.json` (regenerated).
- `web/console/src/routes/(console)/sessions/+page.svelte` (+ the `[id]` detail page header) — title display + inline rename.
- `web/console/src/routes/(console)/playground/[session_id]/+page.svelte` — switcher label + active-session rename + event-driven list refresh.
- `docs/site/protocol/methods.md` / `types.md` / `events.md` (regenerated).
- `scripts/smoke/phase-157.sh`; `RFC-001-Harbor.md` §6.9 (amended in the plans PR); `docs/glossary.md`.

## Public API surface

- `sessions.SessionRegistry.SetTitle(ctx context.Context, id string, ident identity.Identity, title string) error` (manual semantics; empty clears).
- `sessions.TitleSource` (`TitleSourceUnset` / `TitleSourceAuto` / `TitleSourceManual`) — the `Auto` constant is declared here for Phase 158's internal writer; nothing produces it in this phase.
- Wire: `sessions.set_title` method; `SessionRow.title` / `title_source`.

## Test plan

- **Unit:** `SetTitle` semantics table (set / re-set / clear / trim / oversize / control-chars / unknown id / identity mismatch / closed-session allowed); old-blob round-trip additivity; `projectRow` field copy; handler identity-discipline tests mirroring `sessions_handler_delete_test.go` (body-identity mismatch 401, cross-user not-found, oversize 400); event payload content-free assertion.
- **Integration:** protocol-level round-trip against real in-mem drivers — `set_title` → `sessions.list`/`inspect` reflect it → clear → gone; a foreign `(tenant, user)` caller is refused; erased session's title unrecoverable. (Also exercised by the wave-v1.12 E2E.)
- **Conformance:** N/A — no driver seam change (sessions ride the StateStore typed-wrapper; the StateStore conformance suite is untouched).
- **Concurrency / leak:** extend the registry's D-025 stress — N≥100 concurrent `SetTitle`+`ListSnapshots`+`Touch` against one registry under `-race`, distinct sessions show no title bleed; two concurrent `SetTitle` on ONE session serialize (last-write-wins, never a torn record).

## Smoke script additions

- live-server: `sessions.set_title` on the dev session → `sessions.inspect` returns the title + `title_source=manual`; empty-title clear round-trip; >200-rune title → 400.

## Coverage target

- `internal/sessions`: 85%
- `internal/protocol/transports/stream`: existing package target maintained

## Dependencies

- 73c (sessions.list/inspect + Sessions page), 106 (Playground page), 118 (D-223 lockstep gate), 130 (the delete-path identity discipline this mirrors), 155 (erasure suites that must stay green).

## Risks / open questions

- The `(tenant, user)` write scope (vs. own-session-only like erasure) is the one deliberate widening — it is metadata-only, matches the scope `sessions.list` already reads at, and is recorded in D-288; the §6 reviewer should attack it (the E2E asserts cross-user/cross-tenant refusal).
- `<option>` text can't carry rich truncation — the switcher relies on native option rendering; acceptable, the tooltip lives on the Sessions page.

## Glossary additions

- "Session title" (docs/glossary.md, same PR).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] Concurrent-reuse: registry D-025 stress extended (N≥100, `-race`) as above
- [ ] Integration test wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, runs under `-race`
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: N/A — none departed
