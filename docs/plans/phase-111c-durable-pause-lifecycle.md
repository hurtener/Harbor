# Phase 111c — Durable pauses + pause lifecycle

## Summary

The pause/resume primitive ships durability machinery that nothing turns on,
and a lifecycle with no end. Three confirmed gaps (SDK friction audit §3):
(1) `pauseresume.WithCheckpointStore`
(`internal/runtime/pauseresume/coordinator.go:82-84`) has **zero production
consumers** — both assemblies construct the Coordinator without a store
(`cmd/harbor/cmd_dev.go:670`, `harbortest/devstack/devstack.go:697`) while the
`stateStore` is in scope a few lines up; every pause is process-memory-only.
(2) Even with a store, a rehydrated pause could not restore planner state:
`steering.RunLoop.requestPause` persists `Trajectory: nil`
(`internal/runtime/steering/runloop.go:670`, with an in-code "later-phase
concern" comment — this is the later phase). (3) No pause GC/expiry exists:
`DecisionTimeout` (`pauseresume/decision.go:45-51`) is vocabulary with no
producer ("Phase 50 does not yet emit this"), `Resume` is the ONLY
checkpoint-deletion path (`coordinator.go:234-244`), and cancel-while-paused
orphans records forever.

Phase 111c closes all three: thread the run's Trajectory into `requestPause`;
wire `WithCheckpointStore(stateStore)` in both assemblies; ship a
`WithMaxParkDuration` option + an exported sweeper over the existing
List/Resume surface that resumes expired pauses with `DecisionTimeout` —
giving that enum value its first producer. The durability proof is the E2E:
pause → construct a NEW Coordinator over the SAME store → Resume → trajectory
restored.

## RFC anchor

- RFC §3.3 — the unified pause/resume primitive (the durable pause record;
  the typed `Decision` marker incl. `timeout`, D-096).
- RFC §6.3 — Steering and pause/resume (the Coordinator contract; pause-state
  serialization `format_version: 1`; the tool-context split + `ErrToolContextLost`
  fail-loud rule).
- RFC §6.11 — StateStore (the §4.4 persistence seam the checkpoint store
  deliberately reuses — D-067: no parallel persistence driver).

## Briefs informing this phase

- brief 02 — planner + steering + HITL (pause-state serialization fail-loud;
  the pause record's durability expectations)
- brief 05 — state, tasks, artifacts, sessions (the StateStore checkpoint
  surface — `SavePlannerCheckpoint`/`LoadPlannerCheckpoint` — pauses ride on)

## Brief findings incorporated

- **brief 02 §pause serialization.** "When a pause record is serialised … the
  planner can pause holding a callback, get serialised to a state store, and
  be resumed in a different process" — the WHOLE design premise of the
  checkpoint store assumes the pause record actually reaches a store. Today
  it never does on any production path; this phase makes the brief's premise
  true.
- **brief 02 §fail-loud serialization.** Pause-state serialization must fail
  loudly (`ErrUnserializable`) rather than silently dropping context. The
  trajectory-threading change re-runs this gate: a trajectory carrying a
  non-serialisable payload must raise `ErrUnserializable` at pause time, and
  the §11 mandatory test covers it.
- **brief 05 §StateStore as the persistence floor.** "The persistence floor
  for everything else" — checkpointing pauses through StateStore (not a new
  driver seam) is the settled D-067 shape; this phase wires the existing
  floor, it does not mint storage.

## Findings I'm departing from (if any)

None.

## Ship-time deviations (§4.3, recorded 2026-06-10 — D-200)

- **Sweeper scan is registry-internal, not `Coordinator.List`.** The
  Risks section's anticipated conflict materialised: `List` is
  §6-identity-scoped by design (empty `TenantIDs` projects the
  caller's own tenant; cross-tenant filters must NAME tenants under
  `AdminScoped`) — there is no "all tenants" wildcard, and a
  maintenance actor cannot enumerate tenants it has never seen.
  Rather than widening §6, the shipped sweeper lives in the
  `pauseresume` package and snapshots the registry directly (value
  copies under the mutex — the same discipline `List` uses), while
  every MUTATION goes through the public `Coordinator.Resume` under
  the pause's OWN identity triple: scope check, handle re-attach,
  checkpoint delete, and `pause.resumed` emit all run unmodified. No
  storage-level identity filter is bypassed; no elevated List shape
  is minted. Recorded in D-200 §5.
- **Known V1 boundary: crash-orphaned checkpoints are not proactively
  scanned.** The sweeper reaps pauses live in the process registry
  (which covers the audit's cancel-while-paused finding — the
  acceptance floor). A checkpoint orphaned by a PROCESS CRASH is
  rehydrated on demand (`Status` / `Resume`) but not swept until
  something rehydrates it: `state.StateStore` has no scan-by-kind
  surface, and adding one is a §9 RFC conversation, not a quiet
  widening. Recorded in D-200 §5. **RESOLVED (2026-06-10, D-207):**
  the §9 conversation happened — RFC §6.11 gained the one
  explicitly-elevated maintenance scan (`StateStore.ListKind`, three
  drivers + conformance suite), and every sweep pass now rescues
  crash-orphaned `pauseresume.checkpoint:` rows into the registry
  (`rescanCrashOrphans`) so the unchanged expired-scan + public
  `Resume` path reaps them at deadline. This boundary no longer
  exists.
- **Timeout-wake plumbing (additive).** The "waiting run terminates"
  criterion needs the parked RunLoop to OBSERVE the out-of-band reap.
  Shipped as: an identity-scoped bus subscription while parked
  (primary; the canonical `pause.resumed` event) plus a coarse
  `Coordinator.Status` re-check (delivery-independent backstop; the
  only channel on a bus-less RunLoop). `pauseresume.Status` gains an
  additive `Decision` field so the observer distinguishes `timeout`
  from a legitimate out-of-band resume. Non-timeout out-of-band
  resumes deliberately do NOT wake the park (no collision with 111b's
  OAuth completion leg).
- **No eager cancel-time release.** The plan marked it bonus; the
  shipped floor is the sweeper-at-deadline backstop (asserted in the
  E2E). The cancel path is untouched.

## Goals

- **Trajectory threading.** `steering.RunLoop.requestPause` passes the run's
  live trajectory into the `PauseRequest` instead of `nil` (the RunLoop
  already holds it — `RunSpec.Base.Trajectory`). Serialization failures
  surface as `ErrUnserializable` verbatim (the existing contract, now
  actually exercisable with real trajectories).
- **Checkpoint-store wiring.** The one assembly constructs the Coordinator
  with
  `pauseresume.New(pauseresume.WithBus(bus), pauseresume.WithCheckpointStore(stateStore))`:
  Wave B's 110d (promoted `assemble.Assemble` — D-197) has merged, so the
  wiring lands once there — cmd + devstack inherit it as thin callers (no
  D-094 hand-mirror; §17.6's F1 lesson closed by construction).
- **Durability E2E.** Pause a run (real planner `RequestPause` path) →
  construct a NEW Coordinator over the SAME StateStore (simulated restart) →
  `Resume(token, ...)` → the checkpointed trajectory is restored and
  available to the resumed run. Plus the destructive-Resume contract pinned
  by `coordinator.go:234-244`'s godoc stays intact (resumed = deleted from
  store; asserted, not "fixed").
- **Pause lifecycle — the sweeper.**
  - `pauseresume.WithMaxParkDuration(d time.Duration)` Coordinator option:
    pauses carry an expiry derived from `PausedAt + d` (zero = no expiry,
    today's behaviour, the default).
  - An exported sweeper — `pauseresume.RunSweeper(ctx, coord, opts...) error`
    (or `Coordinator.StartSweeper`; implementor picks the shape that keeps
    the Coordinator a compiled artifact per D-025) — periodically walks the
    existing `List` surface (`pauseresume.go:249`) and, for each pause past
    its max-park deadline, calls `Resume(token, DecisionTimeout, payload)` —
    **`DecisionTimeout`'s first producer**. The resulting `pause.resumed`
    event carries the typed `timeout` marker wire consumers already know how
    to read (D-096).
  - A timed-out pause is terminal for the waiting run: it surfaces as a
    constraints-conflict outcome (the `DecisionTimeout` godoc's "resumed it
    to surface a constraint-conflict"), never a silent unpark-and-continue.
  - Cancel-while-paused stops orphaning records: the sweeper is the backstop
    that reaps them at deadline; an explicit cancel-time release is in scope
    if the implementor finds a clean hook on the existing cancel path (both
    close the audit's "orphans records forever" finding; the sweeper alone
    is the acceptance floor).
  - Both assemblies start the sweeper (config-gated:
    `pauseresume.max_park_duration` in `harbor.yaml`, default documented;
    sweeper joins the closer chain — goroutine cancellable + joined on
    shutdown per §5 concurrency rules).
- **Honest config surface.** New `harbor.yaml` fields documented in this
  plan + `examples/harbor.yaml` + validated in `internal/config` (§4.2
  rule 7): `pauseresume.max_park_duration` (duration; 0 = never expire),
  `pauseresume.sweep_interval` (duration; sensible default, e.g. 1m —
  implementor-owned).

## Non-goals

- No cross-process resume — the handle registry is process-local at V1 (RFC
  §6.3, settled); the durability E2E's "new Coordinator, same store" proves
  restart-shaped rehydration of the SERIALIZABLE half. Re-attaching
  non-serialisable handles across processes stays out of scope; a resume
  that needs a lost handle fails loud with `ErrToolContextLost` (existing
  contract, asserted in the E2E's failure mode).
- No new persistence driver, schema, or migration beyond what the checkpoint
  store already writes through StateStore (D-067).
- No Console pause-management UI changes — the pause list page already reads
  `pauses.list`; timeout resumes arrive as canonical `pause.resumed` events
  with the `timeout` Decision (zero Console work by construction).
- No per-pause-reason TTL tiers (one `max_park_duration` knob at V1.1.x;
  tiering is a follow-up if operators ask).

## SDK-consumer reachability

A headless consumer gets the full surface without the binary:
`pauseresume.New(WithBus(bus), WithCheckpointStore(store), WithMaxParkDuration(d))`
plus `RunSweeper(ctx, coord)` are all plain exported calls over the §4.4
StateStore seam — no config file, no Protocol. The
`docs/recipes/steer-and-resume-a-run.md` recipe (introduced by sibling 111b)
gains the durability + expiry section in this phase: how to construct a
durable coordinator, what survives a restart (serializable half), what fails
loud (`ErrToolContextLost`, `ErrUnserializable`), and who reaps abandoned
pauses. The two phases touch different recipe sections; merge order is
irrelevant.

## Acceptance criteria

- [x] `requestPause` threads the run's Trajectory into `PauseRequest`
      (`runloop.go:670`'s `Trajectory: nil` + its "later-phase" comment are
      gone); a checkpoint written under a store-backed Coordinator contains
      the serialized trajectory (`format_version: 1`).
- [x] **§13 primitive-with-consumer:** `WithCheckpointStore` gains its first
      production consumers — BOTH assemblies (cmd + devstack mirror, or the
      promoted `Assemble`) construct the Coordinator with the store, in the
      same phase. `DecisionTimeout` gains its first producer (the sweeper),
      in the same phase.
- [x] §11 mandatory fail-loud test: a pause whose trajectory/payload carries
      a non-serialisable value raises `ErrUnserializable` loudly at
      `Request` time — no silent nil, no silently-empty checkpoint.
- [x] Durability E2E: pause → NEW Coordinator over the SAME store → `Resume`
      → trajectory restored and handed to the resumed run; the
      destructive-Resume contract (resumed ⇒ checkpoint deleted; fresh
      Coordinator returns `ErrPauseNotFound` post-resume) re-asserted.
- [x] `WithMaxParkDuration` + the exported sweeper ship; an expired pause is
      resumed with `Decision: timeout`; the `pause.resumed` event carries the
      typed marker; the waiting run terminates as a constraints-conflict
      outcome (never silently continues); the checkpoint is deleted (no
      orphan).
- [x] Cancel-while-paused no longer leaks forever: at minimum the sweeper
      reaps the record at deadline (asserted); an eager cancel-time release
      is bonus, documented if shipped.
- [x] Sweeper lifecycle is clean: started by the assemblies (config-gated),
      cancellable via ctx, joined on shutdown, goroutine-leak test green.
- [x] Config fields `pauseresume.max_park_duration` + `sweep_interval`
      validated, documented in `examples/harbor.yaml`, defaults pinned.
- [x] `scripts/smoke/phase-111c.sh` asserts the surface (see Smoke script
      additions).
- [x] D-200 (reserved; logged when the phase ships) records: trajectory
      threading, store wiring, sweeper shape, the timeout-is-terminal call.

## Files added or changed

- `internal/runtime/steering/runloop.go` — `requestPause` trajectory
  threading.
- `internal/runtime/pauseresume/coordinator.go` — `WithMaxParkDuration`;
  expiry stamped on the pause record.
- `internal/runtime/pauseresume/sweeper.go` — **NEW** exported sweeper over
  List/Resume; `DecisionTimeout` emission.
- `internal/runtime/pauseresume/sweeper_test.go` — **NEW**.
- `internal/runtime/assemble/assemble.go` — `WithCheckpointStore(stateStore)`
  and `WithMaxParkDuration` on the one Coordinator construction
  (`pauseresume.New`, the merged 110d assembly site — D-197); sweeper start
  and closer-chain join. cmd + devstack inherit as thin callers.
- `internal/config/config.go` + `validate.go` — the two new
  `pauseresume.*` fields + validation.
- `examples/harbor.yaml` — documented fields.
- `test/integration/phase111c_durable_pause_test.go` — durability E2E +
  timeout E2E + `ErrUnserializable` + `ErrToolContextLost` failure modes.
- `docs/recipes/steer-and-resume-a-run.md` — durability + expiry section
  (file introduced by 111b; created here if 111c merges first).
- `scripts/smoke/phase-111c.sh` — real assertions.
- `docs/decisions.md` — D-200 (reserved; logged when the phase ships).
- `docs/plans/README.md` — status flip on ship.

## Public API surface

- `pauseresume.WithMaxParkDuration(d time.Duration) Option`.
- `pauseresume.RunSweeper(ctx context.Context, coord Coordinator, opts ...SweeperOption) error`
  (or the Coordinator-method shape; one exported entry either way).
- Config: `pauseresume.max_park_duration`, `pauseresume.sweep_interval`.
- Behavioural surface: `pause.resumed` events with `Decision: timeout` now
  occur in production (wire consumers already typed for it — D-096).

> Scope note: "public" here is module-internal — `internal/` packages are not
> importable by external modules (the recorded reason `harbortest/` lives at
> the top level). This surface is stable for in-module consumers (cmd,
> harbortest, examples); external-team embedding needs the future
> facade/export RFC (audit §5 / Wave D), out of scope for this phase.

## Test plan

- **Unit:** expiry stamping under `WithMaxParkDuration` (controllable clock —
  the package already takes one; NEVER `time.Sleep` per §11); sweeper
  selects only expired pauses; sweeper Resume payload carries the
  audit-safe timeout facts; zero-duration = never-expires default.
- **Integration:** `test/integration/phase111c_durable_pause_test.go` — real
  drivers (state sqlite — durability deserves a file-backed proof — events
  inmem, real react planner emitting `RequestPause` or the runloop's pause
  path), the restart-shaped durability E2E, the timeout E2E, identity
  propagation on every pause event, failure modes: `ErrUnserializable` at
  Request + `ErrToolContextLost` on handle-lost resume.
- **Conformance:** checkpoint round-trip asserted against all three
  StateStore drivers via the existing conformance harness (the checkpoint
  payload is StateStore-opaque; the assertion is store-agnostic
  save/load/delete parity).
- **Concurrency / leak:** sweeper-vs-Resume race — N concurrent legitimate
  Resumes while the sweeper runs over the same Coordinator under `-race`
  (a pause must resolve exactly once; the loser gets the documented
  `ErrPauseNotFound`/already-resolved error, never a double `pause.resumed`);
  goroutine baseline restored after sweeper shutdown; N≥100 concurrent
  Request/Resume against one store-backed Coordinator (extends the existing
  D-025 suite to the store-backed shape).

## Smoke script additions

`scripts/smoke/phase-111c.sh`:

- Static: `WithCheckpointStore` has a non-test caller under `cmd/` (or the
  promoted assembly) — the audit's regression grep.
- `go test ./internal/runtime/pauseresume/... -run 'Sweeper|Durab'` green;
  `go test ./test/integration/ -run Phase111c` green.
- Live (when classified live-server): with a tiny
  `max_park_duration` fixture, a parked pause eventually surfaces a
  `pause.resumed` event with `decision: timeout` on the events stream.
  404/405/501 → SKIP pre-phase.

## Coverage target

- `internal/runtime/pauseresume`: 90% (existing high bar; sweeper fully
  covered).
- `internal/runtime/steering`: 85% (the requestPause delta covered).

## Dependencies

- 50 (unified pause/resume primitive), 51 (pause durability/serialization
  machinery — `format_version: 1`, checkpoint store seam), 07/15/16
  (StateStore drivers, transitively).
- The D-192 steering-dispatch fix (Wave A): the resumed run's re-entry path
  must be live for the E2E's "resumed run continues" assertions.

## Risks / open questions

- **Staging note (Wave C):** the 111 band parallelizes freely once Wave B
  Stage 1 (110a + 110c) merges; 111c has no hard 110-band dependency (the
  Coordinator construction site moves into `Assemble` if 110d lands first —
  §17.6 covers the transition); all six 111-band phases are mutually
  independent.
- **Trajectory size at pause time.** Long trajectories make checkpoints
  heavy; StateStore handles blobs, but the implementor should assert a sane
  upper bound in the E2E (and note that 111e's compression, when configured,
  naturally bounds what a pause snapshots — independent phases, compounding
  value).
- **Timeout-is-terminal semantics.** Treating `DecisionTimeout` as a
  constraints-conflict terminal mirrors D-071's REJECT posture (a deadline
  the human missed is a constraint the planner can't resolve). If the owner
  prefers re-enter-and-replan on timeout, that is a planner-policy RFC
  conversation — flagged here so it isn't silently decided in code; the
  plan's recommendation stands as written.
- **Sweeper scope across identities.** `List` is identity-scoped; the
  sweeper is a runtime-internal maintenance actor and needs the elevated
  list shape (the registry `WithControlScope` precedent) — NOT a bypass of
  identity filtering in storage. Implementor verifies the existing List
  surface supports the maintenance scan without widening any §6 rule; if it
  can't, pause-and-ask before adding an elevated path.

## Glossary additions

- **Max park duration** — the operator-configured ceiling on how long a
  pause may stay parked before the runtime resumes it with the typed
  `timeout` Decision (`pause.resumed`, D-096) and terminates the waiting run
  as a constraints-conflict. Zero = never (default). Add to
  `docs/glossary.md`.
- **Pause sweeper** — the exported maintenance loop
  (`pauseresume.RunSweeper`) that walks the Coordinator's List surface and
  reaps expired pauses; `DecisionTimeout`'s first producer. Add to
  `docs/glossary.md`.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] Cross-session isolation: pause records + checkpoints + sweeper actions
      stay identity-scoped (asserted under concurrency)
- [x] **Primitive + consumer in the same wave (§13):** `WithCheckpointStore`
      gets its production consumers AND `DecisionTimeout` gets its first
      producer, both exercised end-to-end with tests — checked.
- [x] Pause/resume serialization test asserts `ErrUnserializable` loudly
      (§11 mandatory)
- [x] Concurrent-reuse + sweeper-race tests pass (N≥100, `-race`)
- [x] Integration test wires real drivers end-to-end, asserts identity
      propagation, covers ≥1 failure mode, runs under `-race`
- [x] Config fields documented in plan + example config + validated
- [x] Glossary updated
- [x] D-200 filed when the phase ships
