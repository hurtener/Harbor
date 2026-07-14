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
  `ClosedAt`, `ClosedReason` cleared; immutable identity preserved; `LastSeen`
  refreshed to now; the record re-added to the live `openSessions` index and the
  `(tenant, user)` discovery catalog. The durable events / state / memory that
  were left intact at close are unchanged, so the conversation resumes with full
  history.
- Reopen of an **erased** session is terminal and fails loud with
  `ErrReopenAfterErase` — never a silent empty-start (§5 fail-loud, §7
  right-to-erasure). The check is race-safe against a concurrent / interrupted
  erasure.
- Reopen preserves **all** of §6 multi-isolation: identity-mandatory,
  identity-immutable (`ErrIdentityMismatch` on a stored-vs-ctx identity mismatch),
  cross-tenant reuse still `ErrSessionIDReuse`. No new identity-downgrade knob.
- A content-free `session.reopened` canonical event (SafePayload) is emitted on
  every successful reopen, so the Console live view + audit see the resumption.
- Same-wave §13 consumer: the Console Playground switcher and Sessions page let a
  user resume a closed session (a `start` on a closed id now succeeds instead of
  erroring) and refresh the session list on `session.reopened`.

## Non-goals

- No change to the GC sweep cadence or the idle-TTL policy. Reopen refreshes
  `LastSeen`, which resets the idle-TTL clock naturally; the GC hard-cap
  interaction is a documented open question (see Risks), not reworked here.
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
- [ ] **History intact after reopen:** the session's durable event stream +
  StateStore records + memory are byte-unchanged across a close→reopen cycle —
  proven by a `state.history` (D-254) read-back that returns the same events
  before and after, and a memory read that returns the same entries.
- [ ] **Reopen of an erased session fails loud:** a session that completed
  `session.erase` (Phase 130) returns `ErrReopenAfterErase` from `Open` /
  `EnsureOpen` — never a silent fresh empty session, never resurrected data. An
  erasure that is IN-FLIGHT / interrupted (a pending erasure ledger present) also
  returns `ErrReopenAfterErase`.
- [ ] **Identity-mismatch reopen rejected:** a reopen whose ctx identity's
  `(tenant, user)` disagrees with the stored record returns `ErrIdentityMismatch`
  (defence-in-depth even though the StateStore load is triple-keyed).
- [ ] **Cross-tenant reopen = `ErrSessionIDReuse`:** `SessionID=S` closed under
  tenant A, then `Open`ed under tenant B, returns `ErrSessionIDReuse` (the
  existing `idIndex` guard), NOT a reopen and NOT an `ErrReopenAfterErase`.
- [ ] **`session.reopened` emitted:** a canonical, registered, content-free
  `SafePayload` event (`SessionReopenedPayload{SessionID, ReopenedAt,
  PriorClosedReason}`) is published under the session's own identity on every
  successful reopen; a test asserts the marshalled payload carries no field
  beyond those three (no title, no user content).
- [ ] **`ErrReopenAfterClose` retired from the reopen path:** `Open` /
  `EnsureOpen` no longer return "reopen forbidden" for a closed non-erased
  record. The protocol-side `ErrSessionReopenAfterClose` mapping is retired; the
  `SessionEnsurerAdapter` maps `sessions.ErrReopenAfterErase` →
  `protocol.ErrSessionReopenAfterErase` → `CodeInvalidRequest` (same wire code
  the retired reopen-after-close used — no new wire error CODE, no
  `ProtocolVersion` bump). `Touch` on a still-closed session keeps a loud
  read-only guard (renamed `ErrSessionClosed` — a caller must `start`/reopen
  before touching; Touch is not a reopen entry point).
- [ ] **Race-safe erased check:** reopen's `load record → check erased →
  re-activate → save` runs inside one registry `r.mu` critical section, and the
  erasure cascade's terminal-tombstone write is folded into the SAME
  `r.mu`-serialized destructive step (mirroring the catalog erasure-vs-open
  serialization, `internal/sessions/catalog.go:82-149`), so reopen sees EITHER
  pre-erasure (re-activates) OR post-erasure (tombstone present →
  `ErrReopenAfterErase`) — never a torn interleave that resurrects mid-deletion
  data.
- [ ] **Full D-223 lockstep in the same PR** for the new `session.reopened`
  event: registered event type + `SessionReopenedPayload` in
  `internal/sessions/events.go`; the join row in
  `cmd/harbor-gen-protocol-docs/events.go`; the TS mirror + regenerated
  `wire-manifest.gen.json` via `make protocol-ts-gen`; regenerated
  `docs/site/protocol/events.md` via `make protocol-docs-gen`. `ProtocolVersion`
  unbumped (additive).
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
  erased terminal); new `ErrReopenAfterErase` sentinel; retire
  `ErrReopenAfterClose` (or repurpose its Touch use as a renamed `ErrSessionClosed`
  read-only guard).
- `internal/sessions/registry.go` — `Open`: replace the `if stored.Closed { …
  ErrReopenAfterClose }` branch with a re-activation branch (check erased →
  `ErrReopenAfterErase`; else clear `Closed`/`ClosedAt`/`ClosedReason`, verify
  `sameIdentity` → `ErrIdentityMismatch`, refresh `LastSeen`, re-add to
  `openSessions` + catalog, `unfenceSession`, `save`, emit `session.reopened`);
  `EnsureOpen` — the closed-record branch now returns the re-activated session
  instead of `ErrReopenAfterClose`; `Touch` closed-guard renamed.
- `internal/sessions/events.go` — `EventTypeSessionReopened` +
  `SessionReopenedPayload` (SafeSealed; session id + reopened-at + prior close
  reason), registered in `init()`.
- `internal/sessions/erasure.go` — the terminal-tombstone write (a durable,
  content-free `erasureTombstoneKindPrefix` record under the actor's
  observability scope — the SAME scope that survives `DeleteScope` — written as
  part of `completeErasure`'s success criteria and retained; sibling of the
  `session.erased` record-of-fact) and a reopen-facing `isErased(ctx, id)` helper
  (checks the pending ledger OR the terminal tombstone).
- `internal/protocol/errors.go` — retire `ErrSessionReopenAfterClose`, add
  `ErrSessionReopenAfterErase` (maps to `CodeInvalidRequest`) + its
  `mapSessionEnsureError` arm.
- `internal/runtime/serve/session_ensurer.go` — translate
  `sessions.ErrReopenAfterErase` → `protocol.ErrSessionReopenAfterErase`; drop the
  reopen-after-close arm.
- `cmd/harbor-gen-protocol-docs/events.go` — `session.reopened` join row.
- `web/console/src/lib/protocol/*.ts`, `client.ts`, `wire-manifest.gen.json`
  (regenerated) — the `SessionReopenedPayload` event type.
- `web/console/src/routes/(console)/playground/[session_id]/+page.svelte`,
  `web/console/src/routes/(console)/sessions/+page.svelte` — resume affordance +
  `session.reopened` list refresh.
- `docs/site/protocol/events.md` (regenerated).
- `scripts/smoke/phase-176.sh`; `RFC-001-Harbor.md` §6.9 (amended in the plans
  PR); `docs/glossary.md`; `docs/decisions.md` (D-312).

## Public API surface

- `sessions.ErrReopenAfterErase` (sentinel; reopen of an erased/erasing session).
- `sessions.Registry.Open` / `EnsureOpen` — behaviour change: a closed
  non-erased record re-activates and returns the session (was
  `ErrReopenAfterClose`).
- Wire: `session.reopened` canonical event; `SessionReopenedPayload` fields
  (`session_id`, `reopened_at`, `prior_closed_reason`).
- `protocol.ErrSessionReopenAfterErase` (maps to `CodeInvalidRequest`).

## Test plan

- **Unit:** reopen re-activation table (GC-closed / explicitly-closed → open,
  same `OpenedAt`, refreshed `LastSeen`, re-indexed); erased-session reopen →
  `ErrReopenAfterErase` (completed erasure via tombstone AND in-flight erasure via
  pending ledger); identity-mismatch → `ErrIdentityMismatch`; cross-tenant →
  `ErrSessionIDReuse`; `session.reopened` payload content-free assertion
  (marshalled-bytes grep); `Touch`-on-closed keeps the loud guard.
- **Integration (binding — §17.1; `Deps` names shipped subsystems):**
  `test/integration/session_reopen_test.go` — REAL drivers on the seam
  (`state/drivers/durable` + inmem, `events/drivers/durable`, `memory` real
  driver, the `CascadeEraser`): (1) open → emit turns/state → close (and a second
  case: GC reap) → `EnsureOpen` re-activates → a `state.history` read returns the
  SAME events and memory returns the SAME entries (history intact); (2)
  erase → reopen fails loud `ErrReopenAfterErase` (≥1 failure mode); (3)
  cross-tenant reopen `ErrSessionIDReuse` + identity-mismatch `ErrIdentityMismatch`
  rejection; identity propagation asserted through every layer; run under `-race`.
  On the DURABLE driver specifically, prove history survives the reap→reopen (the
  untrimmed-log guarantee).
- **Conformance:** N/A — no new persistence driver seam; sessions ride the
  StateStore typed-wrapper and the StateStore conformance suite is untouched. The
  erasure tombstone rides the existing `Save`/`Load`/`Delete` surface all drivers
  already implement.
- **Concurrency / leak:** extend the registry's D-025 stress — N≥100 concurrent
  `Open`(reopen) + `Close` + `ListSnapshots` + `Touch` against one registry under
  `-race`, distinct sessions show no cross-talk; the erasure-vs-reopen race
  (`reopen(S)` concurrent with `erase(S)`) resolves to EXACTLY one of
  {re-activated, `ErrReopenAfterErase`} with no resurrected-data outcome and no
  torn record (pinned by a test verified to FAIL against a non-serialized draft,
  run `-race -count=5`).

## Smoke script additions

- live-server: `start` (control) on a session id that was previously closed →
  succeeds and a subsequent `state.history` returns the prior turns; `start` on a
  session id that was erased → `CodeInvalidRequest` (reopen-after-erase);
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

- **Terminality mechanism (THE reviewer scrutiny point — this changes a Settled
  decision).** "Erased is terminal" must hold across a FULLY-CONVERGED erasure,
  but the existing erasure **pending ledger** is deleted on erasure success
  (`completeErasure` → `deleteLedger`), so a completed erasure leaves NO ledger to
  key on. The pending ledger alone therefore only covers the IN-FLIGHT /
  interrupted window (the actual resurrection-race window). To make the binding AC
  "erase → reopen fails loud" hold after a converged erasure, this phase adds a
  durable, content-free **erasure tombstone** (retained under the actor's
  observability scope — the same scope that already carries the durable
  `session.erased` record-of-fact and survives the erased triple's
  `DeleteScope`). Reopen fails loud on EITHER a pending ledger OR a tombstone.
  **The reviewer must confirm the tombstone honours right-to-erasure:** it does —
  it carries the SAME content-free shape as the already-retained `session.erased`
  event (session id + timestamp + deletion counts, zero user content), so it adds
  no new retained information about a deleted user's data. The rejected
  alternative — "a converged erasure frees the id, so a reopen of a
  fully-erased id starts a FRESH empty session (no resurrection, GDPR-honest)" —
  is arguably also defensible, but it makes the "erase → reopen fails loud" AC
  unrepresentable and reads as a silent create where the operator expects a loud
  terminal. The plan chooses the loud tombstone; flag for the §6 reviewer.
- **GC hard-cap interaction.** The hard cap is measured from `OpenedAt` (30 days).
  Reopen preserves `OpenedAt`, so a session opened >30 days ago, reaped by the
  hard cap, then reopened, would be re-reaped on the next sweep — at odds with the
  always-resumable consumer-chat goal. Reopen refreshes `LastSeen` (idle-TTL
  resets cleanly), but the hard cap is not addressed here. Recommended follow-up:
  evaluate the hard cap relative to `LastSeen` (or a `LastReopenedAt` stamp) for
  reopened sessions, so an actively-resumed conversation is never hard-capped out
  from under a live user. Named follow-up, not this phase.
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
