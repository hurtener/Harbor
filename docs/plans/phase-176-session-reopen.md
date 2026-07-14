# Phase 176 — Session reopen: re-activate a closed session so a consumer chat resumes

## Summary

RFC §6.9 was Settled at "reopen-after-close is forbidden — clients open a new
session." The consumer-chat / white-label product model needs conversations to
be **always resumable**: a user returns days later and sends a new message on the
SAME conversation. This phase implements the amended §6.9 (D-312): `Open` /
`EnsureOpen` / `start` on a session whose stored record is `Closed=true` (whether
closed explicitly or GC-reaped) **re-activates** it in place — clearing
`Closed`/`ClosedAt`/`ClosedReason`, preserving the immutable identity, refreshing
`LastSeen` — and the durable events / state / memory are already there (untrimmed
in V1), so the conversation continues with its history intact. The single terminal
exception is an **erased** session (`session.erase`, Phase 130): reopen fails loud
with a new `ErrReopenAfterErase` sentinel, never a silent empty-start, honouring
the right-to-erasure contract. A new content-free `session.reopened` canonical
event lets the Console and audit observe the resumption; the Console Playground /
Sessions page gain the reopen affordance as the same-wave §13 consumer.

## RFC anchor

- RFC §6.9
- RFC §5.2
- RFC §6.13
- RFC §7

## Briefs informing this phase

- brief 05
- brief 06

## Brief findings incorporated

- brief 05 §5: "sessions are first-class with their own table" (Harbor rejected
  the audit-event-keyed-by-`session:{id}` compatibility trick). Reopen operates
  on that first-class record — it re-activates the SAME `session.lifecycle`
  StateStore record in place, never a side-channel resurrection, and the durable
  history the record indexes is already present because the record, not the data,
  is what close/GC reap.
- brief 05 §6: "Cross-tenant isolation. Storing an artifact under tenant A and
  attempting to read under tenant B fails. Same for tasks, sessions, memory,
  trajectories." — reopen inherits the full isolation battery: a reopen carries
  the caller's verified `(tenant, user, session)`, the stored record's immutable
  identity must match (`ErrIdentityMismatch`), and a cross-tenant reopen of a
  session id is `ErrSessionIDReuse`, never a reopen.
- brief 06 §5: "avoid logging large payloads … log references" + the
  metrics-cardinality rule against free-form input — extended to the reopen
  event: `session.reopened` is a `SafePayload` carrying only the session id, the
  reopen timestamp, and the (operator/GC-set, closed-set) prior close reason —
  never user content. Consumers refetch the projection / windowed history
  (D-254) for the conversation body.

## Findings I'm departing from (if any)

- None. This phase is the implementation of an explicit RFC amendment (D-312)
  that supersedes the original §6.9 "reopen-after-close forbidden" invariant.
  The amendment itself is the departure from the prior Settled decision and is
  recorded in `docs/decisions.md` D-312 with the RFC change; the phase does not
  depart from any brief finding.

## Goals

- A closed session record (explicitly closed OR GC-reaped) is **re-activated** by
  `Open` / `EnsureOpen` (the `start` create-on-first-use seam): `Closed`,
  `ClosedAt`, `ClosedReason` cleared; immutable identity AND `OpenedAt` preserved;
  a new `LastReopenedAt` stamped; `LastSeen` refreshed to now; the record re-added
  to the live `openSessions` index and the `(tenant, user)` discovery catalog. The
  durable events / state / memory that were left intact at close are unchanged, so
  the conversation resumes with full history.
- **The GC hard cap does not defeat resume-forever.** The hard cap is measured
  from `max(OpenedAt, LastReopenedAt)` (was `OpenedAt` alone), so a conversation
  opened long ago and reopened today restarts its hard-cap clock from the reopen —
  it is NOT re-reaped on the very next sweep. `OpenedAt` is deliberately NOT
  refreshed (it is the erasure cascade's lifecycle discriminator); the separate
  `LastReopenedAt` stamp is why the cap needs its own field. The idle-TTL clock
  already resets via the refreshed `LastSeen`.
- Reopen of an **erased** session is terminal and fails loud with
  `ErrReopenAfterErase` — never a silent empty-start (§5 fail-loud, §7
  right-to-erasure) — **including after the erasure has fully CONVERGED and
  removed the `session.lifecycle` record.** The erased check therefore gates BOTH
  the closed-record branch AND the not-found / fresh-create fall-through of `Open`
  (a converged erase leaves NO record, so a naive reopen would hit `ErrNotFound`
  and mint a fresh empty session — a silent resurrection). The check is race-safe
  against a concurrent / interrupted erasure.
- Reopen preserves **all** of §6 multi-isolation: identity-mandatory,
  identity-immutable (`ErrIdentityMismatch` on a stored-vs-ctx identity mismatch),
  cross-tenant reuse still `ErrSessionIDReuse`. No new identity-downgrade knob.
- A content-free `session.reopened` canonical event (SafePayload) is emitted on
  every successful reopen, so the Console live view + audit see the resumption.
- Same-wave §13 consumer: the Console Playground switcher and Sessions page let a
  user resume a closed session (a `start` on a closed id now succeeds instead of
  erroring) and refresh the session list on `session.reopened`.

## Non-goals

- No change to the GC sweep cadence, the idle-TTL policy, or the hard-cap
  *default*. The hard-cap *measurement* changes (from `OpenedAt` to
  `max(OpenedAt, LastReopenedAt)`) — that is in scope (FAIL-1), not a follow-up —
  but the 30-day default and the sweep interval are untouched.
- No reopen of an ERASED session by any path — that stays terminal.
- No new isolation principal. `agent_id` is not part of the tuple; reopen scopes
  by `(tenant, user, session)` exactly as every other session op.
- No bulk / admin "reopen any session" widening. Reopen is own-identity only,
  the same scope `Open`/`start` already run at.
- No `ProtocolVersion` bump. The reopen behaviour rides the existing `start`
  method; the only new wire type is the additive `session.reopened` event.
- No retroactive backfill or migration — the `session.lifecycle` record shape is
  unchanged (reopen mutates existing fields; the erasure tombstone is an additive
  observability-scope record, see below).

## Acceptance criteria

- [ ] **Reopen a GC-closed session:** a session reaped by the GC sweep
  (`Closed=true`, `ClosedReason="gc:idle"` / `"gc:hard_cap"`) is re-activated by
  `EnsureOpen` / `Open`: the returned session has `Closed=false`, a refreshed
  `LastSeen`, the SAME `OpenedAt` and identity; it re-appears in `openSessions`
  and `ListSnapshots` as open.
- [ ] **Reopen an explicitly-closed session:** a session closed via `Close(...)`
  is re-activated identically; the prior `ClosedReason` is surfaced on the
  `session.reopened` event's `PriorClosedReason` field, then cleared on the
  record.
- [ ] **Hard cap restarts from the reopen (FAIL-1):** the session record gains a
  `LastReopenedAt` field (stamped on every reopen; `OpenedAt` untouched), and the
  GC hard-cap check (`internal/sessions/gc.go`) measures age from
  `max(OpenedAt, LastReopenedAt)` instead of `OpenedAt` alone. Test: open a
  session with `OpenedAt` older than `HardCap`, close it, reopen it, run a GC
  sweep, and assert it is NOT re-reaped (a session with only `OpenedAt` set and no
  `LastReopenedAt` still hard-caps as before — no regression to never-reopened
  sessions).
- [ ] **History intact after reopen:** the session's durable event stream +
  StateStore records + memory are byte-unchanged across a close→reopen cycle —
  proven by a `state.history` (D-254) read-back that returns the same events
  before and after, and a memory read that returns the same entries.
- [ ] **Reopen of an erased session fails loud — including the CONVERGED case
  (FAIL-2):** a session that FULLY COMPLETED `session.erase` (Phase 130) — so the
  `session.lifecycle` record is GONE and `Open` hits `state.ErrNotFound` — returns
  `ErrReopenAfterErase` from `Open` / `EnsureOpen`, NOT a fresh empty session.
  The `isErased(ctx, id)` guard therefore fires on the **not-found / fresh-create
  fall-through path** (before minting a new record) as well as on the
  closed-record branch, under the same `r.mu` hold. An erasure that is IN-FLIGHT /
  interrupted (a pending erasure ledger present, record possibly still there) also
  returns `ErrReopenAfterErase`. The test MUST exercise the converged
  (record-gone → tombstone) case explicitly, not only the in-flight-ledger case.
- [ ] **Identity-mismatch reopen rejected:** a reopen whose ctx identity's
  `(tenant, user)` disagrees with the stored record returns `ErrIdentityMismatch`
  (defence-in-depth even though the StateStore load is triple-keyed).
- [ ] **Cross-tenant reopen = `ErrSessionIDReuse`:** `SessionID=S` closed under
  tenant A, then `Open`ed under tenant B, returns `ErrSessionIDReuse` (the
  existing in-memory `idIndex` guard — best-effort cross-process, see Risks), NOT
  a reopen and NOT an `ErrReopenAfterErase`.
- [ ] **`session.reopened` emitted:** a canonical, registered, content-free
  `SafePayload` event (`SessionReopenedPayload{SessionID, ReopenedAt,
  PriorClosedReason}`) is published under the session's own identity on every
  successful reopen; a test asserts the marshalled payload carries no field
  beyond those three (no title, no user content).
- [ ] **`ErrReopenAfterClose` retired from the reopen path:** `Open` /
  `EnsureOpen` no longer return "reopen forbidden" for a closed non-erased
  record. The protocol-side `ErrSessionReopenAfterClose` mapping is retired; the
  `SessionEnsurerAdapter` maps `sessions.ErrReopenAfterErase` →
  `protocol.ErrSessionReopenAfterErase` → a NEW dedicated wire code
  `CodeSessionErased` (`"session_erased"`, HTTP 409, in
  `internal/protocol/errors/errors.go` — §8: codes are added there and only there;
  a new additive code is NOT a `ProtocolVersion` break). It is MACHINE-BRANCHABLE
  (WARN-A): a consumer-chat client switches on `code == "session_erased"` to route
  the user to "this conversation was deleted — start a new one", which a shared
  `CodeInvalidRequest` (advisory `Message` only — clients are forbidden from
  branching on `Message`) could not support. `Touch` on a still-closed session
  keeps a loud read-only guard (renamed `ErrSessionClosed` — a caller must
  `start`/reopen before touching; Touch is not a reopen entry point).
- [ ] **Documented-surface prose fixed (FAIL — no stale "reopen-after-close is
  forbidden").** Adding `CodeSessionErased` genuinely adds an `errors.md` row, so
  the D-209 regen runs and the same pass FIXES the now-false prose: the generator
  join for `CodeInvalidRequest` (`cmd/harbor-gen-protocol-docs/errors.go`) no
  longer cites "a `start` on a closed session (reopen-after-close is forbidden)",
  the new `CodeSessionErased` row describes the reopen-after-ERASE terminal case,
  and the hand-written choreography page
  `docs/site/protocol/auth-and-identity.md` is rewritten (a `start` on a closed
  session reopens; only an ERASED session is rejected `session_erased`). `make
  protocol-docs-gen` + `make protocol-ts-gen` + `make protocol-ts-types-gen`
  regenerate `errors.md`, `wire-manifest.gen.json`, and the vendorable
  `harbor-protocol.gen.ts` (the wire-surface digest covers error codes — it
  changes; the count goes 12 → 13). The lockstep gate keys `errors.md` rows on
  `errors.Codes()` presence only, never on `When` prose, so the prose fix is a
  human-review obligation this AC pins, not a mechanical catch.
- [ ] **Race-safe erased check — the real ordering invariant (WARN-1):** reopen's
  `load record (or not-found) → isErased check → re-activate-or-mint → save` runs
  inside one registry `r.mu` critical section, so it serializes against the
  erasure cascade's `r.mu`-held `deleteScopeSerialized` (the record clear) and
  `clearErased` (the index clear). The tombstone is NOT atomic with `DeleteScope`
  — it rides a DIFFERENT (observability) scope and is written in `completeErasure`
  — and does not need to be. The binding invariant is instead
  **write-happens-before-delete**: the erasure writes the terminal tombstone
  BEFORE it deletes the pending ledger (`deleteLedger`), so at every instant
  `isErased == (pending ledger present) ∨ (tombstone present)` holds with NO gap —
  a reopen at any interleave point sees at least one of the two and fails loud.
  The tombstone write is SUCCESS-CRITICAL: a failed tombstone `Save` fails the
  erasure loud (wrapped like `ErrErasureRecordFailed`) and MUST NOT proceed to
  `deleteLedger` — otherwise a `tombstone-fails` + `ledger-deleted` interleave
  would open a converged-erasure gap and permit resurrection.
- [ ] **Tombstone write is UNCONDITIONAL per terminal `completeErasure`, outside
  the `recordAlreadyEmitted` emit-skip guard (WARN-B):** `completeErasure` skips
  `emitErased` when `recordAlreadyEmitted` returns true (`erasure.go`), but the
  tombstone `Save` MUST run on every `completeErasure` invocation that reaches the
  terminal step, independent of that guard, and be idempotent (overwrite). A
  converging retry where a prior attempt emitted the event but died before the
  tombstone write would otherwise (if the `Save` were co-located inside the
  `!recordAlreadyEmitted` block) see `recordAlreadyEmitted == true` → skip
  tombstone → run `deleteLedger` → leave NEITHER ledger nor tombstone → a
  reopen-after-erase then silently resurrects. Test: force
  `recordAlreadyEmitted == true` and assert the tombstone is still written BEFORE
  `deleteLedger`.
- [ ] **`isErased` fails CLOSED on a transient Load error (WARN-C):** `isErased`
  returns `(false, nil)` ONLY when BOTH the ledger `Load` AND the tombstone `Load`
  return `state.ErrNotFound`; any other (non-NotFound) `Load` error propagates and
  reopen fails loud — it never mints a fresh session and never re-activates on an
  unverified erased-state read (mirroring `loadLedger`, `erasure.go:665-678`). A
  fail-OPEN collapse to "not erased" on a transient StateStore fault is exactly
  the seam WARN-2 rejects the history-scan for; the point-`Load` must not
  reintroduce it. Test: a `Load` returning a non-NotFound error → reopen returns
  that error, mints nothing.
- [ ] **Full D-223 / D-209 lockstep in the same PR** for BOTH new wire surfaces
  — the `session.reopened` event AND the `CodeSessionErased` error code:
  registered event type + `SessionReopenedPayload` in
  `internal/sessions/events.go`; the event join row in
  `cmd/harbor-gen-protocol-docs/events.go`; the error code constant +
  `canonicalCodes` entry in `internal/protocol/errors/errors.go`, its HTTP-409
  binding in `internal/protocol/transports/control/status.go`, the conformance
  `expectedHTTPStatus` + `errorCodeMatrix` entries + the size-count test
  (`internal/protocol/conformance/`), and the `errors_test.go` / `status_test.go`
  code-set tables. `make protocol-ts-gen` (manifest + digest), `make
  protocol-ts-types-gen` (vendorable module), and `make protocol-docs-gen`
  (`events.md` + `errors.md`) all regenerate and are committed.
  `ProtocolVersion` unbumped (both additive).
- [ ] **§13 Console consumer, same wave:** the Playground switcher offers a
  "Resume" affordance on a closed session that issues `start` on the closed id
  (now succeeding) and hydrates the history via the existing D-254 windowed read;
  the Sessions page shows a resume control on closed rows and refreshes the list
  on a `session.reopened` subscription. Tokens-only styling, `<PageState>`
  contract untouched (D-121).
- [ ] `scripts/smoke/phase-176.sh` OK ≥ 3, FAIL = 0 (start-on-a-closed-session
  round-trip succeeds with history; erased-session start fails; cross-tenant
  start rejected).

## Files added or changed

- `internal/sessions/sessions.go` — invariant-2 doc rewrite (reopen allowed;
  erased terminal incl. converged); new `Session.LastReopenedAt time.Time` field
  (additive JSON, zero-valued on old records — no migration); new
  `ErrReopenAfterErase` sentinel; retire `ErrReopenAfterClose` (repurpose its Touch
  use as a renamed `ErrSessionClosed` read-only guard).
- `internal/sessions/registry.go` — `Open`: (a) the `if stored.Closed { …
  ErrReopenAfterClose }` branch becomes a re-activation branch (call
  `isErased` → `ErrReopenAfterErase`; else verify `sameIdentity` →
  `ErrIdentityMismatch`, clear `Closed`/`ClosedAt`/`ClosedReason`, stamp
  `LastReopenedAt`, refresh `LastSeen`, keep `OpenedAt`, re-add to `openSessions` +
  catalog, `unfenceSession`, `save`, emit `session.reopened`); (b) the not-found /
  fresh-create fall-through (`registry.go:182`, after `state.ErrNotFound`) FIRST
  calls `isErased` — a converged erasure left no record but a tombstone, so this
  path must fail loud `ErrReopenAfterErase` before minting a fresh session — all
  under the existing `r.mu` hold. `EnsureOpen` — the closed-record branch now
  returns the re-activated session instead of `ErrReopenAfterClose`; `Touch`
  closed-guard renamed.
- `internal/sessions/gc.go` — the hard-cap check reads
  `max(OpenedAt, LastReopenedAt)` instead of `OpenedAt` (FAIL-1); idle-TTL check
  unchanged (already `LastSeen`-relative).
- `internal/sessions/events.go` — `EventTypeSessionReopened` +
  `SessionReopenedPayload` (SafeSealed; session id + reopened-at + prior close
  reason), registered in `init()`.
- `internal/sessions/erasure.go` — the terminal-tombstone write (a durable,
  content-free `erasureTombstoneKindPrefix` record under the actor's
  observability scope — the SAME scope that survives `DeleteScope` — written in
  `completeErasure` BEFORE `deleteLedger`, SUCCESS-CRITICAL (a failed `Save` fails
  the erasure loud wrapped like `ErrErasureRecordFailed` and does NOT proceed to
  `deleteLedger`); sibling of the `session.erased` record-of-fact) and a
  reopen-facing `isErased(ctx, id)` helper (an O(1) point-`Load` of the pending
  ledger OR the terminal tombstone — never a bounded history scan, see Risks).
- `internal/protocol/errors/errors.go` — NEW canonical code `CodeSessionErased`
  (`"session_erased"`) + `canonicalCodes` entry (§8: codes added here and only
  here).
- `internal/protocol/transports/control/status.go` — `CodeSessionErased` → HTTP
  409 binding.
- `internal/protocol/conformance/conformance.go` + `internal_test.go` —
  `expectedHTTPStatus` + `errorCodeMatrix` (with the deferred-live-scenario
  comment, mirroring `CodeSessionRunning`) + the size-count `12 → 13`.
- `internal/protocol/errors/errors_test.go`,
  `internal/protocol/transports/control/status_test.go` — the `wantCodes` / wire
  / status-mapping code-set tables gain `CodeSessionErased`.
- `internal/protocol/errors.go` — retire `ErrSessionReopenAfterClose`, add
  `ErrSessionReopenAfterErase` (maps to `CodeSessionErased`) + its
  `mapSessionEnsureError` arm.
- `internal/runtime/serve/session_ensurer.go` — translate
  `sessions.ErrReopenAfterErase` → `protocol.ErrSessionReopenAfterErase`; drop the
  reopen-after-close arm.
- `internal/runtime/serve/coverage_test.go` — the
  `TestSessionEnsurerAdapter_SentinelTranslation` case at `:522`/`:554` references
  the retired `protocol.ErrSessionReopenAfterClose`; update it to the new
  reopen-after-erase translation (NIT-1 — a compile-time reference that must move).
- `cmd/harbor-gen-protocol-docs/errors.go` — the `CodeInvalidRequest` join no
  longer cites the (now-removed) "reopen-after-close is forbidden"; new
  `CodeSessionErased` join row (the FAIL prose fix).
- `cmd/harbor-gen-protocol-docs/events.go` — `session.reopened` join row.
- `docs/site/protocol/auth-and-identity.md` — hand-written choreography prose
  rewritten: a `start` on a closed session reopens; only an ERASED session is
  rejected `session_erased` (the FAIL prose fix).
- `web/console/src/lib/protocol/*.ts`, `client.ts`, `wire-manifest.gen.json`
  (regenerated — `session.reopened` event + `session_erased` code + new digest).
- `examples/protocol-clients/event-viewer-ts/harbor-protocol.gen.ts` (regenerated
  vendorable module — the new code + digest; `make protocol-ts-types-gen`).
- `web/console/src/routes/(console)/playground/[session_id]/+page.svelte`,
  `web/console/src/routes/(console)/sessions/+page.svelte` — resume affordance +
  `session.reopened` list refresh; branch on `code == "session_erased"` for the
  "conversation deleted — start fresh" path.
- `docs/site/protocol/events.md`, `docs/site/protocol/errors.md` (regenerated).
- `docs/notes/session-model-contract.md` — the closed-session `start` contract
  prose rewritten (a closed-session start REOPENS with history intact; only an
  ERASED id is rejected `session_erased`). No CI gate lints `docs/notes/` prose,
  so this is a same-PR human-review obligation, not a mechanical catch.
- `scripts/smoke/phase-176.sh`; `RFC-001-Harbor.md` §6.9 (amended in the plans
  PR); `docs/glossary.md`; `docs/decisions.md` (D-312).

## Public API surface

- `sessions.ErrReopenAfterErase` (sentinel; reopen of an erased/erasing session,
  including a fully-converged erase).
- `sessions.Session.LastReopenedAt time.Time` (additive record field; drives the
  `max(OpenedAt, LastReopenedAt)` hard-cap measurement).
- `sessions.Registry.Open` / `EnsureOpen` — behaviour change: a closed
  non-erased record re-activates and returns the session (was
  `ErrReopenAfterClose`); a converged-erased id fails loud on the fresh-create
  path (was a silent fresh session).
- Wire: `session.reopened` canonical event; `SessionReopenedPayload` fields
  (`session_id`, `reopened_at`, `prior_closed_reason`).
- Wire: `errors.CodeSessionErased` (`"session_erased"`, HTTP 409) — the
  machine-branchable code for reopen-after-erase.
- `protocol.ErrSessionReopenAfterErase` (maps to `CodeSessionErased`).

## Test plan

- **Unit:** reopen re-activation table (GC-closed / explicitly-closed → open,
  SAME `OpenedAt`, stamped `LastReopenedAt`, refreshed `LastSeen`, re-indexed);
  **hard-cap-restarts-from-reopen** (open with `OpenedAt` > `HardCap`, close,
  reopen, GC sweep → NOT re-reaped; a never-reopened over-cap session still reaps
  — no regression); **erased-session reopen → `ErrReopenAfterErase` on BOTH the
  converged/record-gone path (via tombstone, the not-found fall-through) AND the
  in-flight path (via pending ledger)**; identity-mismatch → `ErrIdentityMismatch`;
  cross-tenant → `ErrSessionIDReuse`; `session.reopened` payload content-free
  assertion (marshalled-bytes grep); `Touch`-on-closed keeps the loud guard;
  erasure-side: a forced tombstone-`Save` failure fails the erasure loud and does
  NOT delete the pending ledger (the write-before-delete success-critical
  invariant); `recordAlreadyEmitted == true` STILL writes the tombstone before
  `deleteLedger` (WARN-B — the unconditional-write case); `isErased` with a
  non-NotFound `Load` error propagates and reopen mints nothing (WARN-C — fail
  closed).
- **Integration (binding — §17.1; `Deps` names shipped subsystems):**
  `test/integration/session_reopen_test.go` — REAL drivers on the seam
  (`state/drivers/durable` + inmem, `events/drivers/durable`, `memory` real
  driver, the `CascadeEraser`): (1) open → emit turns/state → close (and a second
  case: GC reap) → `EnsureOpen` re-activates → a `state.history` read returns the
  SAME events and memory returns the SAME entries (history intact); (2) a
  fully-CONVERGED `session.erase` (record gone) → reopen fails loud
  `ErrReopenAfterErase`, NOT a fresh empty session — the primary AC, plain
  sequential (≥1 failure mode); (3) cross-tenant reopen `ErrSessionIDReuse` +
  identity-mismatch `ErrIdentityMismatch` rejection; (4) hard-cap-restarts: an
  over-cap session reopened then GC-swept is NOT re-reaped. Identity propagation
  asserted through every layer; run under `-race`. On the DURABLE driver
  specifically, prove history survives the reap→reopen (the untrimmed-log
  guarantee).
- **Conformance:** N/A — no new persistence driver seam; sessions ride the
  StateStore typed-wrapper and the StateStore conformance suite is untouched. The
  erasure tombstone rides the existing `Save`/`Load`/`Delete` surface all drivers
  already implement.
- **Concurrency / leak:** extend the registry's D-025 stress — N≥100 concurrent
  `Open`(reopen) + `Close` + `ListSnapshots` + `Touch` against one registry under
  `-race`, distinct sessions show no cross-talk; the erasure-vs-reopen race
  (`reopen(S)` concurrent with `erase(S)`) resolves to EXACTLY one of
  {re-activated, `ErrReopenAfterErase`} with no resurrected-data outcome and no
  torn record — and the race harness MUST include the interleave where the erase
  CONVERGES (record gone, tombstone written) before reopen's mint, asserting
  `ErrReopenAfterErase` there (a race test that only hits the closed-record/ledger
  window masks the not-found-path gap green — FAIL-2). Pinned by a test verified
  to FAIL against a non-serialized / not-found-path-ungated draft, run
  `-race -count=5`.

## Smoke script additions

- live-server: `start` (control) on a session id that was previously closed →
  succeeds and a subsequent `state.history` returns the prior turns; `start` on a
  session id that was erased → HTTP 409 `session_erased` (reopen-after-erase);
  cross-tenant start on a closed id → rejected. (404/405/501 → SKIP so the script
  coexists with pre-176 builds.)

## Coverage target

- `internal/sessions`: 85% (maintain the phase-157 floor; the reopen path + the
  tombstone helper are covered by the unit + integration + D-025 stress).
- `internal/runtime/serve`: existing package target maintained (the ensurer-adapter
  sentinel translation gains one arm, drops one).
- `internal/protocol/transports/stream`: existing target maintained.

## Dependencies

- 130 (`session.erase` — the terminal exception + the erasure ledger/tombstone
  scope reopen checks against), 155 (the erasure-audit-integrity cascade the
  tombstone folds into — must stay green), 125/D-254 (`state.history` windowed
  read — the read path the reopened conversation is reduced from; used by the
  integration test to prove history intact), 73c (`sessions.list`/Sessions page —
  the §13 consumer surface), 106 (Playground page — the reopen affordance
  consumer), 118 (D-223 lockstep gate for the new event).

## Risks / open questions

- **Terminality mechanism — SETTLED as fail-loud (the reviewer scrutiny point on
  a Settled-decision amendment).** "Erased is terminal" must hold across a
  FULLY-CONVERGED erasure, but the existing erasure **pending ledger** is deleted
  on erasure success (`completeErasure` → `deleteLedger`), so a completed erasure
  leaves NO ledger to key on. The pending ledger alone therefore only covers the
  IN-FLIGHT / interrupted window. This phase adds a durable, content-free
  **erasure tombstone** (retained under the actor's observability scope — the same
  scope that carries the durable `session.erased` record-of-fact and survives the
  erased triple's `DeleteScope`). Reopen fails loud on EITHER a pending ledger OR a
  tombstone. **The tombstone honours right-to-erasure:** it carries the SAME
  content-free shape as the already-retained `session.erased` event (session id +
  timestamp + deletion counts, zero user content), adding no new retained
  information about deleted data. The product call is SETTLED (not open): fail
  loud (`ErrReopenAfterErase` → `CodeInvalidRequest`) over a silent fresh session —
  reopen-after-erase is near-universally a stale-client mistake to surface; a
  silent empty session is indistinguishable from data-loss ("where did my
  conversation go?"); it matches §5/§13. A "this conversation was deleted — start
  a new one" Console affordance builds ON TOP of the loud error, not in place of it.
- **Why a dedicated tombstone rather than scanning the existing `session.erased`
  event (WARN-2).** The durable `session.erased` record-of-fact is ALSO retained +
  content-free (untrimmed in V1), and `recordAlreadyEmitted` already scans it — so
  why not have reopen query THAT instead of a new record type? Because a
  terminality guard must FAIL CLOSED, and the event scan cannot: (a) it is
  **capability-gated** — the scan goes through the optional
  `events.HistoryReplayer` seam, which returns `false` when the bus does not
  implement it (a guard that silently returns "not erased" when a capability is
  absent is fail-OPEN — a resurrection); (b) it is **bounded** — `recordAlreadyEmitted`
  scans only `erasureDedupeScanLimit=512` recent events, a dedupe window, not a
  terminality guarantee (an erasure older than the window would read as "not
  erased"); (c) it is **future-retention-prunable** (the Phase 163 retention
  track). A dedicated tombstone is a StateStore point-`Load` — mandatory on all
  three drivers, fail-closed O(1), never pruned. The tombstone is the right call.
- **GC hard-cap interaction — FIXED IN THIS PHASE (FAIL-1), not a follow-up.** The
  hard cap was measured from `OpenedAt` alone, so a session opened >`HardCap` ago,
  reopened today, would be re-reaped within one `SweepInterval` (~15 min) on EVERY
  deployment (`HardCap<=0` restores the 720h default — there is no disable),
  making "resume an old conversation forever" FALSE and causing Closed↔open
  flicker on the Console. This phase adds a `Session.LastReopenedAt` stamp and
  measures the hard cap from `max(OpenedAt, LastReopenedAt)`. `OpenedAt` is
  deliberately NOT refreshed on reopen — it is the erasure cascade's lifecycle
  discriminator (`erasureLedgerRecord.SessionOpenedAt`) and the idempotent
  re-emit key; mutating it would corrupt erasure discrimination. That separation
  is exactly why the cap needs its own field. Pinned by an AC + test
  (reopen an over-cap session, sweep, assert not re-reaped).
- **In-mem driver honesty (dev floor).** On the in-mem StateStore, a session's
  records live in bounded process memory and do not survive a restart; a reopen
  after a dev-server restart re-activates the record shape but the durable history
  the durable driver would preserve is gone. This is the D-074-style "not durable
  across restarts" honesty — durable/Postgres is the real reopen target. The plan
  does not over-index on the dev floor; the integration test proves the guarantee
  on the DURABLE driver.
- **Erased-id reuse count skew.** The erasure cascade already discriminates an
  abandoned prior lifecycle of a reused session id via the `OpenedAt` lifecycle
  stamp; the tombstone is keyed per session id and is terminal, so a reopened
  (never-erased) id and an erased id never collide. A future "re-mint an erased id
  as a brand-new conversation" product need would require an explicit operator
  action (not a reopen) and a new decision.
- **Cross-tenant reuse guard is best-effort cross-PROCESS (NIT-2).** Invariant 3
  (`ErrSessionIDReuse`) is enforced by the in-memory `idIndex` map, which a fresh
  process hydrates lazily (only for the `(tenant, user)` a request touches, via
  the discovery catalog). So immediately after a restart, before the colliding
  tenant's catalog is hydrated, a cross-tenant reuse of the SAME session-id string
  is not caught by `idIndex` — but it is STILL isolation-SAFE: the StateStore
  record is keyed by the full triple, so tenant B loads/writes a DIFFERENT key and
  never sees tenant A's data (no leak, no reopen of A's session). The limitation is
  only that the loud `ErrSessionIDReuse` rejection is best-effort across a process
  boundary; the isolation guarantee is not. Reopen introduces no new exposure here —
  it inherits the existing guard unchanged. Stated so a reviewer does not read
  "reuse rejected" as a cross-process guarantee.
- **Serially-reopened sessions have intentionally-unbounded absolute lifetime
  (NIT-2 — accepted V1 semantic, no knob).** Because the hard cap resets to
  `max(OpenedAt, LastReopenedAt)` on each reopen, a conversation touched at least
  once per hard-cap window never hard-caps — its absolute since-creation age is
  unbounded by design (that is the whole point: resume forever). The idle-TTL
  still collects genuinely abandoned sessions (no `LastSeen` refresh → reaped as
  before), so there is no zombie accumulation. An operator with an
  absolute-age compliance requirement (e.g. "no conversation older than N days,
  regardless of activity") has no control lever in V1. This is INTENDED, stated so
  it is a conscious acceptance rather than a silent gap; a since-creation
  retention knob is a possible follow-up, deliberately not added (no new policy
  surface this phase).
- **Tombstone inherits the `<erasure-audit>` sentinel charset aliasing (NIT-4).**
  The tombstone rides the same reserved observability session id
  (`erasureAuditSession = "<erasure-audit>"`, `internal/sessions/erasure.go:31-38`)
  the ledger + `session.erased` record-of-fact already use, which rests on the
  standing assumption that upstream token/session-header validation rejects a
  caller-supplied session id equal to the angle-bracket sentinel. This is
  PRE-EXISTING, now slightly widened (the sentinel scope is additionally a reopen
  terminality surface). No new mitigation is added here; the possible
  identity-layer charset guard is the same cross-cutting follow-up the erasure
  code already names, acknowledged rather than silently inherited.

## Glossary additions

- "Session reopen" (docs/glossary.md, same PR).
- "Erasure tombstone" (docs/glossary.md, same PR).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
  (reopen preserves the triple; cross-tenant + identity-mismatch rejection
  pinned)
- [ ] **Reusable-artifact concurrent-reuse test:** the registry stays a
  concurrent-reuse artifact — the D-025 stress is extended to N≥100 concurrent
  reopen/close/list under `-race`, plus the reopen-vs-erase race test. See
  AGENTS.md §5 + §11 + D-025.
- [ ] **Integration test (§17.1):** real drivers end-to-end (durable + inmem
  state, durable events, real memory, real `CascadeEraser`), identity propagation
  asserted, ≥1 failure mode (erased-reopen loud + cross-tenant reject), under
  `-race`.
- [ ] If new vocabulary: glossary updated ("session reopen", "erasure tombstone")
- [ ] If a brief finding was departed from: N/A — none departed; the RFC-decision
  amendment is recorded in D-312
