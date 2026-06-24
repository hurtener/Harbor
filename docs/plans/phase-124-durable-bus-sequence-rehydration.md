# Phase 124 — Durable event-bus sequence rehydration on restart

## Summary

The StateStore-backed durable event-log driver assigns each published
event a monotonic, gap-free bus sequence from an in-memory counter
(`bus.nextSeq`). That counter is initialised to `0` at construction and
is **never rehydrated** from the persisted log on restart. After a
Runtime restart the first `Publish` re-assigns `Sequence=1,2,3…`, which
collide with the sequence tokens issued before the restart — and a
generic Protocol client that reconnects with a high `Last-Event-ID` then
has every post-restart event **silently skipped** by `Replay` (which
returns only sequences strictly greater than the cursor) until the
counter climbs back past the old high-water mark. This phase fixes the
correctness bug by recovering `nextSeq` at construction from the
persisted head records (the global max persisted sequence across all
sessions), so post-restart sequences stay strictly monotonic and every
reconnecting Protocol client resumes the **persisted** history without a
silent gap.

The same correctness fix surfaces a second instance of the *identical*
silent-skip class, which this phase closes in the same change so the
repair is whole. The transient, non-persisted bus-internal notices
(`audit.admin_scope_used`, `audit.redaction_failed`) advance the shared
`nextSeq` in `publishInternal` (`internal/events/drivers/durable/durable.go:689-697`)
and are rendered by the SSE transport with `id:<Sequence>`
(`internal/protocol/transports/stream/frame.go:74-76`). A live client can
therefore anchor its `Last-Event-ID` on a transient notice's sequence.
After restart the recovery floors `nextSeq` at the max **persisted**
sequence (transient notices are never persisted, so they are absent from
the head records); the next persisted event re-uses that same token, and
`Replay` — strictly `> cursor` — silently skips it. This phase removes
transient notices from the durable-replay sequence space: `publishInternal`
no longer advances `nextSeq` and assigns the non-replayable sentinel
`Sequence == 0`, and the SSE encoder omits the `id:` line for any event
with no assigned replay position (`Sequence == 0`), so an `EventSource`
keeps its prior `Last-Event-ID` and can never anchor a reconnect cursor on
a transient notice. The two fixes compose: the max sequence ever surfaced
with an `id:` line is exactly the max persisted sequence, the recovery
floors there, and no reconnect — at a persisted token or at a (formerly)
transient one — can be skipped.

## RFC anchor

<!-- Required. List the RFC sections this phase implements. Format exactly: RFC §6.X. -->
- RFC §6.13 — Typed event bus: "Sequence numbering: per-bus monotonic …
  gap-free" and "exact replay when the durable log driver … is
  configured". The gap-free, resumable-across-restart guarantee for the
  durable (persisted) history is the contract this phase repairs. The
  `id:`/`Last-Event-ID` resume cursor is the wire face of that sequence
  numbering, so the SSE-framing half of the fix is in §6.13 scope too.
- RFC §6.11 — StateStore: the `ListKind` explicitly-elevated maintenance
  scan (`ListScope{MaintenanceScoped: true}`) is the read the recovery
  uses to enumerate the durable log's per-session head records at boot.
  The recovery is another maintenance-scan consumer alongside the pause
  sweeper's crash-orphan rescan and the durable task/distributed/agent-
  config recovery paths.

## Briefs informing this phase

<!-- Required. -->
- brief 06

## Brief findings incorporated

<!-- Quote specific findings the plan adopts. -->
- brief 06 (events / observability / devx — the durable-replay /
  Last-Event-ID resumability requirement): a late or reconnecting
  subscriber that replays from a cursor must see the full, gap-free
  history across a Runtime restart — replay reads from the StateStore,
  not an in-memory ring, *precisely so* a restart does not lose history.
  The driver's package doc states this verbatim ("a late subscriber that
  connects after the Runtime was rebuilt against the same StateStore sees
  the full, gap-free history"). The current code honours that for
  PRE-restart events but breaks it the moment a post-restart `Publish`
  re-uses a sequence; this phase closes the gap so the brief's
  resumability promise holds for the post-restart timeline too.
- brief 06 (fail-loud event plumbing): the bus must surface problems
  rather than mask them — the driver already returns
  `stream.replay_unavailable` rather than serving a gap, and fails loud
  on a torn head/entry write. Recovery follows the same posture: a scan
  or decode failure at construction fails the boot, it does not silently
  fall back to a zero counter.

## Findings I'm departing from (if any)

<!-- Required (can be "None"). -->
- The original durable-driver design note records that "there is no
  list/scan method" on the StateStore, so the log was built from a
  per-session head record plus per-event entry records with **no
  max-sequence recovery at construction**. That assumption is now stale:
  `StateStore.ListKind` (RFC §6.11) added an explicitly-elevated,
  maintenance-scoped, prefix-matched scan after the durable driver
  shipped. This phase deliberately departs from the
  "no recovery possible" stance by using `ListKind` over the head-record
  prefix to derive the global max persisted sequence at boot. The
  departure is logged in `docs/decisions.md` as D-255.
- The durable driver's own package doc + `publishInternal` comment assert
  that transient notices, though "assigned a bus sequence (for live
  ordering)", are "not part of" the durable replay history. The
  adversarial review pinned that this is contradicted by
  `frame.go::encodeEvent`, which emits `id:<Sequence>` for *every* event
  including transient notices — so a live client *can* anchor a reconnect
  cursor on one, exactly the skip this phase repairs one level up. This
  phase departs from the "transient notices are harmless because they are
  never a valid replay anchor" stance: it makes that statement *true* by
  removing transient notices from the replay sequence space (sentinel
  `Sequence == 0`, no `id:` line) rather than leaving the latent skip in
  place. Captured in D-255.

## Goals

<!-- Outcomes, not implementation. -->
- After a Runtime restart against the same StateStore, the durable
  driver's first `Publish` assigns a sequence strictly greater than the
  maximum **persisted** sequence from before the restart — no collision
  with any pre-restart persisted token.
- A Protocol client that reconnects with a `Last-Event-ID` at the
  pre-restart persisted high-water mark receives **every** post-restart
  persisted event via `Replay` (no silent skip), preserving the gap-free
  resumability contract RFC §6.13 promises for the durable history.
- No reconnect cursor can be anchored on a transient, non-persisted
  notice: transient notices carry no assigned replay position and the SSE
  transport emits no `id:` line for them, so the highest sequence a client
  can ever hold as `Last-Event-ID` is a persisted one — which the recovery
  floor strictly exceeds after restart.
- Recovery is correct across multiple sessions sharing one bus: the
  recovered counter is the global maximum across all persisted sessions,
  not a per-session value.
- Recovery is fail-loud: a scan or decode failure at construction surfaces
  as a `New(...)` error (boot fails) rather than silently starting at `0`.

## Non-goals

<!-- Explicit out-of-scope items. -->
- No new Protocol method, REST endpoint, or wire type. This is a
  driver-internal correctness fix plus an additive SSE-framing rule, both
  behind the existing `events.EventBus` / `events.Replayer` surface and
  the existing event-stream transport. (Implies no TS client mirror / no
  `make protocol-ts-gen` regen — see Public API surface.)
- Best-effort ring-buffer mode (no StateStore configured) is unchanged for
  recovery: nothing is persisted, so there is nothing to rehydrate;
  `nextSeq` stays `0` and replay remains non-durable across restarts
  (already loudly warned at construction). Recovery is a durable-mode-only
  step. (The transient-notice `Sequence == 0` change applies in both
  modes — it is a property of `publishInternal`, not of persistence — but
  has no restart-resume consequence in best-effort mode.)
- Transient bus-internal notices (`audit.admin_scope_used`,
  `audit.redaction_failed`) are NOT made replayable. They remain per-call
  live observability; this phase only ensures they never occupy or anchor
  a replay position, never that they survive a restart.
- The SSE-framing rule is scoped to "an event with no assigned replay
  position (`Sequence == 0`) carries no `id:` line." It does not change the
  framing of any event that has a sequence, so the `inmem` driver — whose
  transient notices currently still advance its own counter and emit `id:`
  lines — is behaviourally unchanged by this phase (no persisted history,
  no restart-skip; consistency there is noted, not built). See Risks.
- No change to the keying scheme, the head/entry record shapes, or the
  replay algorithm. Recovery only re-derives the counter's starting value.

## Acceptance criteria

<!-- Binding, testable. -->
- [ ] In durable mode, `durable.New(...)` rehydrates `nextSeq` from the
      persisted log before returning: it enumerates the per-session head
      records via `StateStore.ListKind(ctx, ListScope{MaintenanceScoped:
      true}, "events.durable.head")`, decodes each, and sets `nextSeq` to
      the global maximum sequence found across every record's `Sequences`
      list (0 when the log is empty).
- [ ] A regression test publishes events AFTER a simulated restart (a
      fresh `bus` over the same StateStore) and asserts **no sequence
      collision**: every post-restart `Sequence` is strictly greater than
      the maximum pre-restart `Sequence`.
- [ ] The same test asserts **no silent skip**: a client replaying from a
      cursor at the pre-restart high-water mark (`Cursor{Sequence: N}`,
      `N` = pre-restart max) receives every post-restart event, in
      sequence order, with no gap.
- [ ] Transient-notice skip is closed (the SAME silent-skip class): a
      regression test makes a transient notice the **highest sequence
      surfaced before restart** (publish a persisted event, then trigger
      `audit.admin_scope_used`), restarts, and publishes again — and
      asserts (a) the transient notice carried no replayable cursor
      (`Sequence == 0` and `encodeEvent` emits no `id:` line for it), and
      (b) a client replaying from the cursor it could actually hold (the
      last **persisted** sequence) receives the post-restart event with no
      skip.
- [ ] `publishInternal` assigns the non-replayable sentinel `Sequence == 0`
      to transient notices and does **not** advance `nextSeq` (the shared
      persisted-replay counter) in any mode; live fan-out order is
      unchanged (transient notices still arrive in call order).
- [ ] `stream.encodeEvent` omits the `id:` line for an event with
      `Sequence == 0`, and emits `id:<Sequence>` for every event with a
      sequence ≥ 1 (the first persisted sequence is 1, so 0 is an
      unambiguous "no replay position" sentinel). A unit test in the
      `stream` package asserts both branches.
- [ ] Recovery is global across sessions: with sessions sA and sB
      persisted pre-restart (max sequence in sB), a post-restart publish to
      sA receives a sequence strictly greater than sB's pre-restart max —
      proving the counter floor is the cross-session maximum, not a
      per-session one.
- [ ] Recovery is fail-loud: a StateStore whose `ListKind` returns an
      error, or a head record that fails to decode, makes
      `durable.New(...)` return a wrapped error (boot fails); it never
      silently starts `nextSeq` at `0`. (No silent degradation — CLAUDE.md
      §13.)
- [ ] Best-effort mode (store == nil) skips recovery entirely and is
      behaviourally unchanged (existing loud-warning + ring tests still
      pass).
- [ ] The recovery scan asserts `ListScope{MaintenanceScoped: true}`; with
      the zero scope it would be rejected with
      `ErrMaintenanceScopeRequired` — the recovery sets it explicitly and
      only reads sequence numbers (it acts on no record's mutable state).
- [ ] The concurrent-reuse test for the durable bus still passes under
      `-race` (N≥100 invocations against one shared instance); recovery is
      a single-threaded construction step and introduces no new shared
      mutable field.
- [ ] All pre-existing durable-driver and `stream`-transport tests pass
      (in particular cross-session isolation, replay-cursor, best-effort,
      and the SSE frame-shape tests).

## Files added or changed

<!-- Tree-style. Reference AGENTS.md §3. -->
- `internal/events/drivers/durable/durable.go` —
  - add a `ctx context.Context` first parameter to `New(...)` (it now does
    I/O at construction — CLAUDE.md §5 "ctx is the first parameter of every
    function that does I/O"); thread it to the new recovery step;
  - add a construction-time `recoverNextSeq(ctx)` step in `New(...)`
    (durable mode only) that sets `b.nextSeq` to the recovered global max
    before returning the bus; godoc names the feature ("rehydrate the
    sequence counter from the persisted head records"), not a phase number;
  - update `publishInternal` to assign `Sequence = 0` (the non-replayable
    sentinel) and stop advancing `b.nextSeq`; update its godoc to state
    that transient notices carry no replay position;
  - update the two registered factory closures and `newWithOwnedStore` to
    pass a boot ctx into `New` (see Public API surface for the ctx-boundary
    rationale).
- `internal/protocol/transports/stream/frame.go` — `encodeEvent` omits the
  `id:` line when `ev.Sequence == 0`; godoc explains that an event with no
  assigned replay position carries no SSE id so a reconnecting client never
  anchors `Last-Event-ID` on it (feature-named, no phase/decision number).
- `internal/protocol/transports/stream/frame_test.go` — assert `encodeEvent`
  emits no `id:` line for `Sequence == 0` and emits `id:<n>` for `n ≥ 1`.
- `internal/events/drivers/durable/durable_test.go` — add the
  publish-after-restart regression test(s); add the transient-notice-skip
  regression test; add the multi-session recovery test; add the fail-loud
  recovery test (the existing `failingStore.ListKind` returns `(nil, nil)`
  today, so a NEW stub whose `ListKind` returns an error is required — name
  it `listFailingStore`; also pre-seed a store with a corrupt head record
  for the undecodable-head path). Update existing direct `durable.New(...)`
  call sites for the new ctx parameter. Optionally strengthen the existing
  `TestDurable_ReplayAcrossRestart_NoGaps`.
- `internal/events/drivers/durable/coverage_test.go`,
  `concurrent_test.go`, `openwith_test.go` — mechanical update of direct
  `durable.New(...)` call sites for the new ctx parameter.
- `internal/events/openwith.go` — update the doc comment that quotes the
  `durable.New(cfg, r, store)` signature to the ctx-leading form.
- `internal/events/drivers/durable/record.go` — no shape change; the
  existing `decodeHead` + `headRecord.Sequences` are reused by recovery.
  (Listed for completeness; touched only if a tiny max-of-sequences helper
  lands here rather than in `durable.go`.)
- `test/integration/durable_eventlog_test.go` — update direct
  `durable.New(...)` call sites for the new ctx parameter.
- `scripts/smoke/phase-124.sh` — copied from `scripts/smoke/_template.sh`;
  guards the event-stream resume surface (skips where absent — see Smoke).
- No change to `internal/drivers/prod/prod.go` — the durable driver is
  already registered there; this phase changes its internals only.

## Public API surface

<!-- What other phases depend on. -->
- **No Protocol surface added.** No Protocol method, wire type, or error
  code is added, so there is **no** TS client mirror and **no**
  `make protocol-ts-gen` manifest regeneration (the D-223 lockstep gate is
  not engaged by this phase). The `events.EventBus` / `events.Replayer`
  interfaces are unchanged.
- **Constructor signature change (internal):** `durable.New` gains a
  leading `ctx context.Context`:
  `New(ctx context.Context, cfg config.EventsConfig, r audit.Redactor,
  store state.StateStore, opts ...Option) (events.EventBus, error)`.
  `New` now performs construction-time I/O (the recovery scan), so per
  CLAUDE.md §5 the ctx belongs in the signature; this also makes a slow
  store scan cancellable at startup. Callers are the two registered factory
  closures, `newWithOwnedStore`, `events.OpenWith`, and tests — all updated
  in this PR. `recoverNextSeq` is unexported.
- **ctx-boundary note (the SHOULD-FIX decision).** The `events.Register` /
  `events.RegisterWithDeps` factory contract is ctx-free
  (`func(cfg, r) (EventBus, error)`), so the registered closures bridge the
  recovery ctx with `context.Background()` — the sanctioned §5
  unmanaged-async-boundary case, identical to `newWithOwnedStore`'s existing
  `state.Open(context.Background(), …)` store-open. `newWithOwnedStore`
  reuses the very boot ctx it already creates for `state.Open`. Threading a
  real boot ctx through the `events.Register` factory contract itself is the
  residual gap, tracked as an open question (it would touch the events
  registry contract, beyond this driver-internal fix's blast radius).
- **SSE-framing change is ADDITIVE, no version bump.** `encodeEvent`
  omitting the `id:` line for `Sequence == 0` is a backward-compatible
  refinement of the existing event-stream transport — no method, type, or
  error is added or removed; clients that already treat `Last-Event-ID`
  per the SSE grammar (retain the last seen `id:`) need no change.
  `ProtocolVersion` is held at `0.1.0`. Per
  `internal/protocol/types/version.go`, the additive-vs-breaking taxonomy
  is: Major is bumped only on a *breaking* Protocol change; additive
  surface extensions ship without a bump while V1 is in flight. RFC §5.3 is
  cited here ONLY for the rule that *bumping* `ProtocolVersion` is an RFC
  change — which this phase does not do.

## Test plan

<!-- Categorize: unit / integration / conformance / concurrency / leak / fuzz. -->
- **Unit:**
  - `TestDurable_PublishAfterRestart_NoSequenceCollision`: publish N
    pre-restart, close, re-construct over the same store, publish M more;
    assert every post-restart `Sequence` > pre-restart max and the full
    1..N+M run is collision-free and strictly monotonic.
  - `TestDurable_PublishAfterRestart_ReconnectAtHighWaterMark_NoSilentSkip`:
    after the restart-and-republish above, `Replay(Cursor{SessionID,
    Sequence: preRestartMax})` returns exactly the M post-restart events,
    in order, none skipped.
  - `TestDurable_TransientNoticeIsHighestPreRestartSeq_NoPostRestartSkip`:
    publish one persisted event, then trigger a transient notice
    (`audit.admin_scope_used`) so it is the last thing emitted pre-restart;
    assert the captured transient notice has `Sequence == 0` and that
    `nextSeq` was NOT advanced by it (the next persisted publish still gets
    `lastPersisted+1`); restart, publish, and assert a `Replay` from the
    last **persisted** sequence (the cursor a client could actually hold)
    returns the post-restart event with no skip.
  - `TestStream_EncodeEvent_OmitsIDForZeroSequence` (in the `stream`
    package): `encodeEvent` for an event with `Sequence == 0` produces a
    frame with NO `id:` line; for `Sequence == 7` it produces `id: 7`.
  - `TestDurable_RecoverNextSeq_GlobalAcrossSessions`: persist sA (3
    events) and sB (5 events) pre-restart; after restart a publish to sA
    receives `Sequence == 9` (global max 8 + 1), proving the floor is the
    cross-session maximum.
  - `TestDurable_RecoverNextSeq_EmptyLog_StartsAtZero`: fresh store ⇒
    first published sequence is 1 (recovery of an empty log is a no-op).
  - `TestDurable_RecoverNextSeq_FailsLoudOnScanError`: a new
    `listFailingStore` whose `ListKind` returns an error (the existing
    `failingStore.ListKind` returns `(nil, nil)` and would let recovery
    succeed with an empty log, so it cannot exercise this path) makes
    `durable.New(...)` return a wrapped error.
  - `TestDurable_RecoverNextSeq_FailsLoudOnUndecodableHead`: a store
    pre-seeded with a corrupt `events.durable.head` record makes
    `durable.New(...)` return a wrapped decode error (no silent zero start).
  - `TestDurable_BestEffort_SkipsRecovery`: store == nil ⇒ recovery is not
    invoked; existing loud-warning behaviour intact.
- **Integration:** in-package restart simulation is the binding shape: one
  real `state/drivers/inmem` StateStore survives across two real `durable`
  bus instances (no mocks at the seam), identity triple propagated through
  publish→persist→recover→replay, and the fail-loud path exercised with a
  faulty store. The seam this phase consumes (StateStore ↔ durable bus)
  lives entirely inside the durable package, so the in-package test IS the
  integration test (AGENTS.md §17.2). The existing
  `test/integration/durable_eventlog_test.go` exercises the cross-instance
  restart-over-shared-store path and is updated for the ctx parameter. All
  run under `-race`.
- **Conformance:** N/A — no new interface or driver; the existing
  events-driver behaviour (replay semantics) is unchanged.
- **Concurrency / leak:** the existing durable concurrent-reuse test
  (`concurrent_test.go`, N≥100 against one shared instance, `-race`) must
  still pass; extend it (or add a sibling) so the shared instance is one
  that was recovered from a non-empty log, proving recovery composes with
  concurrent publish/replay without a data race or a goroutine leak.

## Smoke script additions

<!-- scripts/smoke/phase-124.sh assertions. -->
- `scripts/smoke/phase-124.sh`: this is a driver-internal correctness fix
  plus an additive SSE-framing rule, with no new wire method, so the smoke
  guards the observable resume path rather than a new endpoint. Using
  `common.sh` helpers it (a) opens the event stream, (b) records the last
  sequence / `Last-Event-ID` it saw, and (c) issues a replay/reconnect from
  that cursor and `assert_json_*` that the response is well-formed and
  gap-free. Every probe wraps `skip_if_404` so the script SKIPs cleanly on
  builds where the streaming transport is absent (the 404/405/501 → SKIP
  convention, AGENTS.md §4.2). A full restart-and-resume is exercised by the
  unit/integration tests, not the live smoke (restarting the preflight
  server mid-smoke is out of scope); the smoke's job here is to confirm the
  resume surface still answers and shape-validates after this change.

## Coverage target

<!-- Per touched package. -->
- `internal/events/drivers/durable`: ≥ 85% (matches the events subsystem's
  Phase 06 target; this phase adds new branches all of which are tested).
- `internal/protocol/transports/stream`: hold the package's existing
  coverage; the new `encodeEvent` `Sequence == 0` branch is covered by
  `TestStream_EncodeEvent_OmitsIDForZeroSequence`.

## Dependencies

<!-- Phase numbers that must land before this one. -->
- Phase 06 (bus replay + ring buffer + cursor — the `events.Replayer`
  surface this phase keeps correct across restarts).
- Phase 57 (the StateStore-backed durable event-log driver — the artifact
  this phase fixes).
- Phase 60 (the SSE event-stream transport — `encodeEvent` / the
  `id:`/`Last-Event-ID` framing this phase refines).
- Phase 12 / Phase 13 (runtime engine + run loop — the producers whose
  emitted events flow through the durable bus and are resumed by Protocol
  clients).
- (Implicitly consumes RFC §6.11 `ListKind`, which shipped with the
  StateStore; no separate phase gate beyond the durable driver.)

## Risks / open questions

<!-- Real risks. Link RFC §11 Q-N when applicable. -->
- **Transient non-persisted notices — now closed, not deferred.** Before
  this phase, `publishInternal` advanced the shared `nextSeq` for
  `audit.admin_scope_used` / `audit.redaction_failed` and `encodeEvent`
  emitted an `id:` for them, so a live client could anchor `Last-Event-ID`
  on a transient tick that the post-restart recovery floor (max persisted)
  would not exceed — the same silent skip this phase repairs for persisted
  events. This phase removes transient notices from the replay sequence
  space (`Sequence == 0`, no `id:` line) and ships a regression test where a
  transient notice is the highest pre-restart emission. This is NOT recorded
  as a known limitation; it is fixed.
- **Maintenance-scan audit trail.** The boot recovery does a cross-identity
  `ListKind(ListScope{MaintenanceScoped: true}, …)` read. It does NOT emit a
  dedicated audit event — it follows the pause sweeper's crash-orphan
  rescan posture (`internal/runtime/pauseresume/sweeper.go::rescanCrashOrphans`,
  the comparable cross-identity maintenance scan), which records its scan
  via structured `slog` (`InfoContext` on success, `ErrorContext` on
  failure) rather than an `audit.Redactor` event. Recovery does the same: a
  single `Info` boot line (driver, recovered max sequence, session count)
  on success, and a fail-loud wrapped error on scan/decode failure. The scan
  reads sequence numbers only and mutates nothing, so no redaction-bearing
  audit record is warranted.
- **`ListKind` scan cost at boot.** Recovery reads one head record per
  session (the scan matches the `events.durable.head` prefix exactly and
  returns head records only, not entry records). For very large numbers of
  sessions this is an O(sessions) boot read. It is bounded (head records are
  small and one-per-session) and happens once at construction; acceptable
  for V1. If it ever bites, a single persisted "global max" checkpoint
  record is the natural follow-up — noted, not built.
- **Boot context — threaded, with a tracked residual gap.** `New` gains a
  leading `ctx` so the scan is cancellable where a real boot ctx exists
  (`newWithOwnedStore` reuses its `state.Open` ctx). The `events.Register`
  factory contract is ctx-free, so the registered closures bridge with
  `context.Background()` (the §5 unmanaged-boundary case, matching
  `newWithOwnedStore`'s existing precedent). Open question for review:
  thread a real boot ctx through the `events.Register` / `RegisterWithDeps`
  factory contract so the closures stop bridging — deferred to keep this
  correctness fix's blast radius inside the durable driver.
- **Prefix-match exactness.** The recovery scans the literal prefix
  `events.durable.head`; entry kinds use the distinct prefix
  `events.durable.entry/`, so the scan never picks up entry records.
  Verified against the constants in `durable.go` (`kindHead`,
  `kindEntryPrefix`).
- **`inmem` driver consistency.** The SSE `Sequence == 0` rule is
  driver-agnostic, but the fix to stop assigning replayable sequences to
  transient notices lands only in the durable driver (the one with a
  restart-skip). The in-memory driver's transient notices still advance its
  own counter and emit `id:` lines; that has no restart-resume consequence
  (the inmem bus is not durable across restarts), so it is out of scope —
  noted as a possible later consistency cleanup, not built.

## Glossary additions

<!-- New vocabulary. -->
- `sequence rehydration` — the construction-time recovery of the durable
  bus's monotonic sequence counter from the persisted head records, so
  post-restart sequences stay strictly greater than any pre-restart token.
  Add to `docs/glossary.md` in the same PR.
- `non-replayable sequence (Sequence 0)` — the sentinel assigned to a
  transient, non-persisted bus notice: it occupies no replay position and
  the SSE transport emits no `id:` line for it, so a reconnecting client
  cannot anchor `Last-Event-ID` on it. Add to `docs/glossary.md`.

## Pre-merge checklist

<!-- Same checklist that gates PR review (AGENTS.md §14 + drift-audit). -->
- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §6.13`, `RFC §6.11`, `brief 06`) resolve
- [ ] Coverage on `internal/events/drivers/durable` ≥ 85%; `stream`
      coverage held with the new `encodeEvent` branch covered
- [ ] If multi-isolation paths changed: cross-session isolation test passes
      (the existing `TestDurable_Replay_CrossSessionIsolation` plus the new
      global-recovery-across-sessions test).
- [ ] Reusable-artifact concurrent-reuse test passes — N≥100 concurrent
      invocations against one shared (recovered) instance under `-race`, no
      data races / context bleed / cancellation cross-talk / goroutine
      leaks. (The durable bus IS a reusable artifact; this box is binding,
      not N/A.)
- [ ] Integration test exists (in-package restart simulation +
      `test/integration/durable_eventlog_test.go`), wires real drivers
      (`state/drivers/inmem` + `durable`) end-to-end, asserts identity
      propagation, covers ≥1 failure mode (faulty `ListKind`), runs under
      `-race`. (Dependencies name shipped phases, so this box is binding.)
- [ ] New vocabulary added to `docs/glossary.md` (`sequence rehydration`,
      `non-replayable sequence (Sequence 0)`)
- [ ] `ProtocolVersion` unchanged (0.1.0) — the SSE-framing change is
      additive; no TS lockstep engaged.
- [ ] Brief-finding departures justified above + `docs/decisions.md` D-255
      entry filed.

---

## Implementation handoff

Turnkey artifacts for the implementing agent. Copy these verbatim (adjust
only where a `<…>` placeholder is called out).

### (a) Master-plan README index row

Append to the phase index table in `docs/plans/README.md`, matching the
column format `# | Name | Subsystem | RFC § | Deps | Cov. | Status`:

```text
|124 | Durable event-bus sequence rehydration on restart (rehydrate the durable driver's `nextSeq` from the persisted head records at construction via the `ListKind` maintenance scan so post-restart sequences stay strictly monotonic and a client reconnecting at a high `Last-Event-ID` is not silently skipped; also close the same skip class for transient notices — `Sequence 0`, no SSE `id:` line; D-255) | internal/events/drivers/durable | §6.13, §6.11 | 06, 57, 60, 12, 13 | 85% | Pending (V1.6) |
```

### (b) `docs/decisions.md` entry (markdownlint-clean)

Append at the end of `docs/decisions.md`. Note the blank lines around the
`## D-255` heading and around any `---` rule (markdownlint MD022/MD031):

```markdown

---

## D-255 — Durable event-bus sequence counter is rehydrated from the persisted log on restart

**Status:** Accepted.

**Context.** The StateStore-backed durable event-log driver
(`internal/events/drivers/durable`) assigns each event a monotonic,
gap-free bus sequence from an in-memory `nextSeq` counter initialised to
`0` at construction. That counter was never recovered from the persisted
log: after a Runtime restart against the same StateStore the first
`Publish` re-issued `Sequence=1,2,3…`, colliding with pre-restart tokens.
A Protocol client reconnecting with a high `Last-Event-ID` then had every
post-restart event silently skipped by `Replay` (which returns only
sequences strictly greater than the cursor) until the counter climbed back
past the old high-water mark — a direct violation of the §6.13 gap-free,
resumable-across-restart contract. The original driver design noted "there
is no list/scan method" and therefore did no max-sequence recovery; that
assumption was made stale by the later addition of `StateStore.ListKind`
(D-207, RFC §6.11), the explicitly-elevated maintenance scan. The same
silent-skip class had a second instance: transient, non-persisted notices
(`audit.admin_scope_used`, `audit.redaction_failed`) advanced the shared
`nextSeq` and the SSE transport (`stream.encodeEvent`) emitted `id:` for
them, so a live client could anchor `Last-Event-ID` on a transient tick
that the post-restart recovery floor (max persisted) would not exceed.

**Decision.** At construction, in durable mode only, the driver rehydrates
`nextSeq` from the persisted per-session head records: it calls
`ListKind(ctx, ListScope{MaintenanceScoped: true}, "events.durable.head")`,
decodes each head record, and sets `nextSeq` to the global maximum sequence
found across every record's `Sequences` list (0 for an empty log). It is
another maintenance-scan consumer alongside the pause sweeper's
crash-orphan rescan and the durable task / distributed / agent-config
recovery paths. The recovery acts read-only and per-record under each
record's own identity — it only reads sequence numbers, never widens a
mutation scope — and follows the pause sweeper's posture of recording the
cross-identity scan via structured `slog`, not a dedicated audit event. It
is fail-loud: a scan error or an undecodable head record makes `New(...)`
return a wrapped error (boot fails); the driver never silently starts at
`0` (CLAUDE.md §13 "no silent degradation"). Best-effort ring mode
(store == nil) persists nothing and skips recovery. To close the second
instance of the skip class, transient bus-internal notices are removed from
the replay sequence space: `publishInternal` assigns the non-replayable
sentinel `Sequence == 0` and no longer advances `nextSeq`, and
`stream.encodeEvent` omits the SSE `id:` line for any event with
`Sequence == 0`, so a reconnecting client can never anchor `Last-Event-ID`
on a transient notice. `New` gains a leading `ctx context.Context` (it now
does construction-time I/O — CLAUDE.md §5); the ctx-free `events.Register`
factory closures bridge with `context.Background()` (the §5
unmanaged-boundary case, matching `newWithOwnedStore`'s existing
`state.Open` precedent), with threading ctx through the factory contract
itself tracked as a follow-up.

**Consequences.** Post-restart sequences are strictly greater than any
pre-restart persisted token; a client reconnecting at the pre-restart
high-water mark receives every post-restart persisted event with no silent
skip, and no transient notice can be a reconnect anchor. The fix is two
binding regression tests: one that publishes AFTER a simulated restart (the
prior `TestDurable_ReplayAcrossRestart_NoGaps` missed the bug because it
only replayed pre-restart events), and one where a transient notice is the
highest pre-restart emission. Boot does one O(sessions) head-record read; a
single global-max checkpoint record is the noted follow-up if that ever
bites. The SSE-framing change is additive — `ProtocolVersion` stays 0.1.0
(`internal/protocol/types/version.go`: Major bumps only on a breaking
change; RFC §5.3 governs only that *bumping* is an RFC change, which this
phase does not do); no TS lockstep engaged.

**Cross-references.** D-207 (`ListKind` maintenance scan this consumes),
D-028 (the event-bus surface reconciliation), D-025 (concurrent-reuse
contract the recovered bus still satisfies). RFC §6.13, §6.11. brief 06.
Plan: `docs/plans/phase-124-durable-bus-sequence-rehydration.md`.
```

### (c) `scripts/smoke/phase-124.sh` assertions

Start from `scripts/smoke/_template.sh`; source `common.sh`. All probes
wrap `skip_if_404` so the script SKIPs on builds without the streaming
transport. If a new `common.sh` helper is added (e.g. a small SSE
last-id reader), give it a one-line docstring per AGENTS.md §4.2 (item 3).
Assertions to add:

```bash
# Phase 124 — durable bus sequence rehydration / resume surface guard.
# No new wire method: guard that the event-stream resume path still
# answers and shape-validates after the nextSeq-recovery + transient-notice
# (Sequence 0, no SSE id) change.

# 1. Stream surface is reachable (or SKIP if this build has no SSE transport).
skip_if_404 "$(api_url /v1/events/stream)" "events stream" || smoke_skip

# 2. A replay/reconnect from a recorded cursor returns a well-formed,
#    sequence-ordered (gap-free) response — the resumability contract this
#    phase repairs at the driver layer, observed at the wire.
assert_status "$(api_url /v1/events/stream)" 200 "events stream reachable"
# (If the build exposes a replay-by-cursor probe, assert its JSON shape:)
# assert_json_path <replay-response> '.events[0].sequence' "<expected>" \
#   "replay returns sequence-ordered events"

# Full restart-and-resume, and the transient-notice no-id framing, are
# covered by the durable-driver + stream-transport unit/integration tests;
# the live server is not restarted mid-smoke.
```

(Adjust the concrete stream/replay path to match the shipped Phase 60
transport route names at implementation time; keep every probe behind
`skip_if_404`.)

### (d) Master-plan per-phase detail-block stub

Add under the per-phase detail section of `docs/plans/README.md`:

```markdown
### Phase 124 — Durable event-bus sequence rehydration on restart

**Subsystem:** `internal/events/drivers/durable` (+ a small additive
`internal/protocol/transports/stream` framing change).
**RFC:** §6.13 (gap-free, resumable-across-restart sequence numbering +
the `id:`/`Last-Event-ID` resume cursor), §6.11 (the `ListKind`
maintenance scan used to read the head records).
**Deps:** 06 (replay + cursor), 57 (durable driver), 60 (SSE transport),
12/13 (runtime producers). **Coverage:** 85%. **Decision:** D-255.

Fixes a correctness bug for generic Protocol clients: the durable driver's
in-memory `nextSeq` counter was never rehydrated from the persisted log,
so after a Runtime restart the first `Publish` re-used pre-restart
sequence tokens and a client reconnecting with a high `Last-Event-ID` had
every post-restart event silently skipped by `Replay`. Phase 124
rehydrates `nextSeq` at construction from the per-session head records via
`ListKind(ListScope{MaintenanceScoped: true}, "events.durable.head")`,
taking the global max across sessions, fail-loud on scan/decode error, and
recording the cross-identity scan via structured `slog` (the pause-sweeper
posture). It also closes the same skip class for transient notices:
`publishInternal` assigns the non-replayable sentinel `Sequence == 0` and
stops advancing `nextSeq`, and `stream.encodeEvent` omits the SSE `id:`
line for `Sequence == 0`, so no reconnect cursor can anchor on a transient
tick. `New` gains a leading `ctx` (construction now does I/O). Binding
regression tests: publish AFTER a simulated restart, and make a transient
notice the highest pre-restart emission, asserting no collision and no
skip. No Protocol surface change; `ProtocolVersion` held at 0.1.0
(additive SSE refinement). **Risks:** O(sessions) boot read (bounded;
global-max checkpoint is the noted follow-up); ctx bridged with
`context.Background()` at the ctx-free `events.Register` factory boundary
(threading it through the factory contract is the tracked follow-up).
```
