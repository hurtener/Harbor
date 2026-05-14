# Phase 50 — Pause/Resume Coordinator + handle registry

## Summary

Ship `internal/runtime/pauseresume` — Harbor's **ONE** pause/resume primitive (CLAUDE.md §7 rule 4, RFC §3.3 / §6.3). The `Coordinator` interface (`Request` / `Resume` / `Status`) is the single runtime-level coordination point for HITL approval, tool-side OAuth, A2A `AUTH_REQUIRED` / `INPUT_REQUIRED`, and operator/Console `PAUSE`. `Token` is opaque (runtime-owned ULID encoding). A process-local pause registry holds live pause records keyed by `Token`; durability rides on the existing `state.StateStore` (Phase 07) when a checkpoint store is configured — a pause survives Runtime restart **only** when StateStore-backed. The non-serialisable tool-context half re-attaches through the existing `trajectory.HandleRegistry` (Phase 43); a lost handle fails loud with `ErrToolContextLost`. This phase ships the *primitive*; per CLAUDE.md §13 the first `RequestPause`-driven-through-the-Coordinator consumer is folded into Phase 53 (steering wiring) — same wave, Stage 3 — and that obligation is tracked in the "§13 primitive-with-consumer obligation" section below.

## RFC anchor

- RFC §6.3
- RFC §3.3
- RFC §3.4
- RFC §3.5

## Briefs informing this phase

- brief 02

## Brief findings incorporated

- **brief 02 §3 (Pause/Resume — runtime protocol primitives, not a planner return type).** "Pause/Resume — runtime protocol primitives, not a planner return type." Phase 50 lands the `Coordinator` in `internal/runtime/pauseresume`, a runtime package — planners only ever *signal* `RequestPause` (the `Decision` shape, Phase 42 / D-047); the runtime executor (later phase) drives the `Coordinator`. The Coordinator never imports the planner package; it consumes the `planner.PauseReason` enum via a byte-stable typedef bridge (`pauseresume.Reason = planner.PauseReason`) so call sites stay stable — exactly the bridge D-047 sketched.
- **brief 02 §4 (Pause-state serialisation: MUST FAIL LOUDLY) + §5 (Sharp edges #1: Silent context loss on resume).** "It silently drops non-serialisable tool context on resume … Bugs that follow are extremely [hard to trace]." Phase 50 has no silent-degradation path: `Request` with a checkpoint store configured calls `trajectory.Trajectory.Serialize` and propagates `ErrUnserializable` verbatim — a pause whose trajectory cannot serialise is rejected loud, never half-persisted. `Resume` re-attaches handles via `trajectory.HandleRegistry.Get` and propagates `ErrToolContextLost` — never resumes with a `nil` tool context.
- **brief 02 §4 (Handle registry persistence).** "V1: process-local. Resume must run in the same Runtime process. The seam for a distributed handle directory exists (the registry is an interface) but no production driver ships at V1." Phase 50 reuses Phase 43's `trajectory.HandleRegistry` (already an interface with a process-local driver) rather than minting a parallel registry. The Coordinator's own pause registry (Token → live `Pause`) is likewise process-local at V1; the distributed handle/pause directory is a post-V1 RFC concern (RFC §6.3 + RFC §12).
- **brief 02 §6 (Tests required — Pause/resume durability).** "pause → save (in-mem / SQLite / Postgres) → load → resume; trajectory and steering history exact match. Attempt resume with non-recoverable `ToolContext` handle; must return `ErrToolContextLost`." Phase 50's integration test (`test/integration/phase50_durability_test.go`) wires the real `state.StateStore` across all three V1 drivers (in-mem, SQLite, Postgres) and runs the pause→serialise→checkpoint→load→resume round-trip plus the lost-handle negative case.
- **brief 02 §5 (Sharp edges #6: session lock around run/resume).** "The reference [implementation] compensates with a session lock around `run`/`resume`. Harbor's interface requires planners to be safe to use concurrently across runs." Phase 50's `Coordinator` is a D-025 compiled artifact: immutable after construction, per-call state in `ctx` + arguments, the pause registry behind a mutex with documented invariants. Concurrent `Request` / `Resume` / `Status` against one shared `Coordinator` is safe; idempotent re-`Resume` of an already-resumed `Token` is a no-op error (`ErrAlreadyResumed`), never a double-apply.

## Findings I'm departing from (if any)

- **No new §4.4 driver seam for the checkpoint store.** A literal reading of §4.4 might suggest `pauseresume` needs its own `CheckpointStore` interface + driver registry. Phase 50 deliberately does **not** mint one: durability rides on the existing `state.StateStore` (Phase 07), which is *already* the §4.4 seam for persistence — three V1 drivers (in-mem / SQLite / Postgres) with conformance parity (CLAUDE.md §9). Adding a second persistence seam would be the §13 "two parallel implementations of the same conceptual feature" smell. The Coordinator takes an **optional** `state.StateStore` at construction (`WithCheckpointStore`); when absent, pauses are process-local-only and explicitly do **not** survive restart (the master-plan acceptance criterion: "pauses survive Runtime restart only when StateStore-backed checkpoint is configured"). Recorded as **D-067**.

## Goals

- Ship `internal/runtime/pauseresume` as the canonical home for the unified pause/resume primitive: the `Coordinator` interface (`Request` / `Resume` / `Status`), the `Pause` / `PauseRequest` / `Status` value types, the opaque `Token` type, and the `Reason` typedef bridge onto `planner.PauseReason`.
- `Coordinator.Request(ctx, PauseRequest) (Pause, error)` mints an opaque runtime-owned `Token`, records the pause in the process-local registry, and — when a checkpoint store is configured — serialises the pause record (including `Trajectory.Serialize`) and `state.StateStore.Save`s it. Identity-mandatory at the boundary.
- `Coordinator.Resume(ctx, Token, payload) error` validates the resuming identity scope against the original pause's scope, re-attaches tool-context handles via `trajectory.HandleRegistry`, marks the pause resumed (idempotent — second resume returns `ErrAlreadyResumed`), and clears the checkpoint.
- `Coordinator.Status(ctx, Token) (Status, error)` reports the pause lifecycle state (`StatusPaused` / `StatusResumed`) without mutating it; `ErrPauseNotFound` for an unknown token.
- A pause **survives Runtime restart only when** a StateStore-backed checkpoint store is configured: a fresh `Coordinator` constructed `WithCheckpointStore` rehydrates pause records on demand via the checkpoint load path.
- **Fail-loudly throughout (RFC §3.4):** `ErrUnserializable` from `Trajectory.Serialize` propagates verbatim out of `Request`; `ErrToolContextLost` from `HandleRegistry.Get` propagates verbatim out of `Resume`; missing identity → `ErrIdentityRequired`; unknown token → `ErrPauseNotFound`. No `(nil, nil)` / silent-drop path anywhere.
- `Token` is opaque to clients — runtime-owned ULID encoding; clients never construct or parse it.
- D-025 concurrent-reuse contract: N≥100 goroutines invoking `Request` / `Resume` / `Status` against one shared `Coordinator` under `-race` — no races, no context bleed, no cross-cancellation, no goroutine leaks.
- Integration test wiring the real `state.StateStore` across all three V1 drivers (in-mem / SQLite / Postgres) for the pause→serialise→checkpoint→load→resume round-trip + the lost-handle negative case.
- Coverage on `internal/runtime/pauseresume`: ≥ 90%.

## Non-goals

- **No `RequestPause`-emitting consumer in this PR.** Phase 50 ships the primitive; the first end-to-end consumer that drives `RequestPause` *through the Coordinator* is folded into Phase 53 (steering wiring), same wave, Stage 3. See the "§13 primitive-with-consumer obligation" section. Phase 50's own tests exercise the `Coordinator` directly and thoroughly.
- **No distributed handle / pause directory.** V1 is process-local: resume must run in the same Runtime process. The seam exists (`state.StateStore` is the durability interface; `trajectory.HandleRegistry` is the handle interface) but no distributed driver ships at V1 — post-V1 RFC concern (RFC §6.3 + RFC §12).
- **No new persistence driver seam.** Durability rides on the existing `state.StateStore` (see "Findings I'm departing from"). Phase 50 adds no `internal/runtime/pauseresume/drivers/` tree.
- **No pause-record *serialise contract* authoring.** The fail-loud serialise contract for the pause record's wire shape (`format_version: 1`, the exact JSON envelope) is Phase 51's job (RFC §6.3 "Pause-state serialization format"). Phase 50 consumes `trajectory.Trajectory.Serialize` (Phase 43) and `state.StateStore.Save`'s opaque `Bytes`; Phase 51 deepens the envelope. Phase 50 ships a minimal, forward-compatible checkpoint envelope (`checkpointRecord` with a `FormatVersion int` field defaulting to `1`) so Phase 51 extends rather than rewrites.
- **No steering inbox / control-plane wiring.** The `Control` event taxonomy, the per-run control inbox, and `PAUSE` / `RESUME` steering events are Phase 52 / 53. Phase 50 ships the coordination primitive those phases call into.
- **No Protocol surface.** `Coordinator` is a runtime-internal type. The Protocol's `task.pause` / `task.resume` methods (later phase) project onto the Coordinator; Phase 50 ships no HTTP/Protocol endpoint — `scripts/smoke/phase-50.sh` runs the package test suite under `-race` and skips the (absent) protocol surface per the 404/405/501 → SKIP convention.
- **No A2A `AUTH_REQUIRED` / tool-side OAuth wiring.** Those subsystems *call* the Coordinator in their own phases; Phase 50 ships the primitive they converge on.
- **No fuzzing.** Happy-path / round-trip / durability / negative / concurrent are the gate.

## Acceptance criteria

- [ ] `internal/runtime/pauseresume/pauseresume.go` defines the `Coordinator` interface (`Request` / `Resume` / `Status`), the `Pause` struct (`Token`, `Reason`, `Payload`, `PausedAt`, `Identity`), `PauseRequest`, `Status` (struct: `State`, `Reason`, `PausedAt`, `ResumedAt`), the opaque `Token` string type, and `Reason` as a typedef bridge (`type Reason = planner.PauseReason`) re-exporting the four canonical reason constants.
- [ ] `internal/runtime/pauseresume/coordinator.go` defines the process-local `coordinator` implementation + `New(opts ...Option)` constructor. `Option`s: `WithCheckpointStore(state.StateStore)`, `WithHandleRegistry(trajectory.HandleRegistry)`, `WithClock(func() time.Time)`, `WithBus(events.EventBus)` (optional event emission).
- [ ] `Token` is minted runtime-side via a ULID source; opaque to clients (no exported parse/construct helper beyond the internal generator). `Coordinator.Request` returns a fresh unique `Token` per call.
- [ ] `Coordinator.Request(ctx, PauseRequest) (Pause, error)`: validates identity (wrapped `ErrIdentityRequired` on a partial quadruple); records the pause in the process-local registry; when a checkpoint store is configured, serialises the pause record (calling `req.Trajectory.Serialize()` when a trajectory is supplied) and `Save`s it — propagating `ErrUnserializable` **verbatim** on a non-serialisable trajectory (no half-persist).
- [ ] `Coordinator.Resume(ctx, Token, payload map[string]any) error`: returns wrapped `ErrPauseNotFound` for an unknown token; validates the resuming identity scope matches the original pause's `(tenant, user, session)` (wrapped `ErrScopeMismatch` otherwise); re-attaches tool-context handles via the `trajectory.HandleRegistry`, propagating `ErrToolContextLost` **verbatim** on a lost handle; marks the pause resumed; second `Resume` of the same token returns `ErrAlreadyResumed` (idempotent, no double-apply); clears the checkpoint from the store.
- [ ] `Coordinator.Status(ctx, Token) (Status, error)`: returns the pause lifecycle state without mutation; wrapped `ErrPauseNotFound` for an unknown token; falls back to a checkpoint-store load when the token is absent from the in-memory registry but a checkpoint store is configured (the restart-survival path).
- [ ] A `Coordinator` constructed **without** a checkpoint store: `Request` succeeds (process-local only); a fresh `Coordinator` (simulating restart) returns `ErrPauseNotFound` for that token — pauses explicitly do **not** survive restart without a store.
- [ ] A `Coordinator` constructed **with** a checkpoint store: after `Request`, a fresh `Coordinator` over the *same* store resolves the token via `Status` / `Resume` — the pause survived "restart".
- [ ] `internal/runtime/pauseresume/errors.go` defines sentinels: `ErrIdentityRequired`, `ErrPauseNotFound`, `ErrAlreadyResumed`, `ErrScopeMismatch`. `ErrUnserializable` / `ErrToolContextLost` are *not* redefined — they propagate from `trajectory` and callers reach them via `errors.As`. (A `Coordinator` with no checkpoint store rehydrating an unknown token returns `ErrPauseNotFound` — "absent from the in-mem registry, no store" is genuinely *not found*, not a distinct misconfiguration error; an early draft of this plan listed an `ErrCheckpointStoreRequired` sentinel for that case — the Wave 9 §17.5 audit found it dead and removed it, since the no-store path correctly surfaces `ErrPauseNotFound` already.)
- [ ] Identity-mandatory at every `Coordinator` method boundary (CLAUDE.md §6 rule 9 + D-001). No identity-downgrading knob.
- [ ] D-025 concurrent-reuse test (`internal/runtime/pauseresume/concurrent_test.go`): N≥100 goroutines, distinct identity quadruples, against one shared `Coordinator` — `Request` then `Resume` then `Status` — under `-race`. Asserts no races, no context bleed (each goroutine's pause carries only its own identity/payload), no cross-cancellation (pre-cancelled ctx on a subset honoured per-call), no goroutine leak (baseline `runtime.NumGoroutine` restored).
- [ ] Integration test (`test/integration/phase50_durability_test.go`): real `state.StateStore` across in-mem / SQLite / Postgres drivers; pause→serialise→checkpoint→load→resume round-trip exact-match; lost-handle negative case returns `ErrToolContextLost`; missing-identity negative case returns `ErrIdentityRequired`; runs under `-race`. (Postgres leg skips with a reason when no test database is reachable — matching the existing state-driver integration-test convention.)
- [ ] Pause/resume serialisation test (CLAUDE.md §11 mandatory): a `PauseRequest` whose `Trajectory.ToolContext` carries a non-serialisable handle → `Request` returns `ErrUnserializable` (via `errors.As`), **not** a half-persisted checkpoint.
- [ ] `scripts/smoke/phase-50.sh` runs `go test -race ./internal/runtime/pauseresume/...`, asserts the `Coordinator` interface shape with a static guard, asserts the import-graph guard (`internal/runtime/pauseresume` does **not** import the Console or declare a parallel persistence-driver tree), and skips the (absent) Protocol surface with a reason.
- [ ] `docs/plans/README.md` Phase 50 row flipped `Pending` → `Shipped`; root `README.md` status table updated in sorted position.
- [ ] `docs/decisions.md` D-067 filed; `docs/glossary.md` gains `Pause/Resume Coordinator`, `Token (pauseresume)`, `Pause record`, `Checkpoint store`.
- [ ] Coverage on `internal/runtime/pauseresume` ≥ 90%.

## Files added or changed

```text
docs/plans/phase-50-pauseresume-coordinator.md      (new — this file)
docs/plans/README.md                                (Phase 50 row → Shipped)
docs/decisions.md                                   (D-067 appended)
docs/glossary.md                                    (4 new terms)
README.md                                           (status table row)
internal/runtime/pauseresume/pauseresume.go          (new — Coordinator iface + value types + Reason bridge + package doc)
internal/runtime/pauseresume/coordinator.go          (new — process-local coordinator + New + Options)
internal/runtime/pauseresume/checkpoint.go           (new — checkpointRecord envelope + save/load helpers over state.StateStore)
internal/runtime/pauseresume/errors.go               (new — sentinel errors)
internal/runtime/pauseresume/events.go               (new — typed SafePayload event payloads: pause.requested / pause.resumed)
internal/runtime/pauseresume/pauseresume_test.go     (new — unit: Request/Resume/Status, restart-survival both ways)
internal/runtime/pauseresume/coordinator_test.go     (new — unit: idempotency, scope mismatch, fail-loud paths)
internal/runtime/pauseresume/concurrent_test.go      (new — D-025 N≥100 concurrent-reuse)
test/integration/phase50_durability_test.go          (new — durability across in-mem/SQLite/Postgres + negatives)
scripts/smoke/phase-50.sh                            (new — package test run + interface/import-graph guards)
```

## Public API surface

```go
package pauseresume

// Reason bridges onto the planner-side enum (byte-stable; D-047).
type Reason = planner.PauseReason

// Token is opaque to clients; the runtime owns the encoding.
type Token string

type Pause struct {
    Token    Token
    Reason   Reason
    Payload  map[string]any
    PausedAt time.Time
    Identity identity.Identity
}

type PauseRequest struct {
    Identity   identity.Identity
    Reason     Reason
    Payload    map[string]any
    Trajectory *trajectory.Trajectory // optional; serialised into the checkpoint when a store is configured
}

type State string
const (
    StatusPaused  State = "paused"
    StatusResumed State = "resumed"
)

type Status struct {
    State     State
    Reason    Reason
    PausedAt  time.Time
    ResumedAt time.Time // zero unless State == StatusResumed
}

type Coordinator interface {
    Request(ctx context.Context, req PauseRequest) (Pause, error)
    Resume(ctx context.Context, token Token, payload map[string]any) error
    Status(ctx context.Context, token Token) (Status, error)
}

type Option func(*coordinator)
func WithCheckpointStore(s state.StateStore) Option
func WithHandleRegistry(r trajectory.HandleRegistry) Option
func WithClock(now func() time.Time) Option
func WithBus(b events.EventBus) Option

func New(opts ...Option) Coordinator

var (
    ErrIdentityRequired = errors.New("pauseresume: identity triple incomplete")
    ErrPauseNotFound    = errors.New("pauseresume: pause token not found")
    ErrAlreadyResumed   = errors.New("pauseresume: pause already resumed")
    ErrScopeMismatch    = errors.New("pauseresume: resume identity scope does not match pause")
)
```

## Test plan

- **Unit:** `Request` mints a unique opaque `Token` and records the pause; `Status` reports `StatusPaused`; `Resume` flips to `StatusResumed`; second `Resume` → `ErrAlreadyResumed`; unknown token → `ErrPauseNotFound`; missing identity → `ErrIdentityRequired`; resume with a mismatched `(tenant,user,session)` → `ErrScopeMismatch`. Restart-survival both ways: without a store a fresh `Coordinator` cannot see the token; with a shared store it can. Pause/resume serialisation test (§11 mandatory): non-serialisable trajectory → `ErrUnserializable` propagated, no half-persist.
- **Integration:** `test/integration/phase50_durability_test.go` — real `state.StateStore` across all three V1 drivers; pause→serialise→checkpoint→load→resume round-trip with exact-match trajectory bytes; lost-handle → `ErrToolContextLost`; missing-identity → `ErrIdentityRequired`. Real `trajectory.HandleRegistry` on the seam (no mocks per §17.3). Runs under `-race`. Postgres leg skips-with-reason when no DB is reachable.
- **Conformance:** N/A — Phase 50 ships one Coordinator implementation. The §4.4 driver-conformance pattern applies to the *checkpoint store*, which IS `state.StateStore` — already covered by `internal/state/conformancetest`. No new conformance suite.
- **Concurrency / leak:** `concurrent_test.go` — D-025 N≥100 goroutines, distinct identities, `Request`→`Resume`→`Status` against one shared `Coordinator` under `-race`; per-goroutine identity-bleed assertions; pre-cancelled ctx on a subset; baseline `runtime.NumGoroutine` restored after all goroutines join.

## Smoke script additions

- `go test -race -count=1 ./internal/runtime/pauseresume/...` passes (unit + D-025 concurrent-reuse) → `ok`.
- Static guard: `internal/runtime/pauseresume/pauseresume.go` declares the `Coordinator` interface with `Request` / `Resume` / `Status` methods (grep guard) → `ok`.
- Import-graph guard: `internal/runtime/pauseresume/` does not import any Console package and does not declare a `drivers/` persistence tree (durability rides on `state.StateStore`) → `ok`.
- Skip: Phase 50 ships no Protocol/HTTP surface (the `task.pause` / `task.resume` Protocol methods land in a later phase) → `skip` with reason, per the 404/405/501 → SKIP convention.

## Coverage target

- `internal/runtime/pauseresume`: 90% (master-plan Phase 50 target).

## Dependencies

- Phase 07 — `state.StateStore` (the durability backing for checkpointed pauses).
- Phase 09 — envelopes / identity quadruple plumbing (`identity.Identity` / `identity.Quadruple` flow through `ctx`).
- Phase 13 — cancellation (the Coordinator honours `ctx` cancellation per-call; cancelling one `Request` never affects another — D-025 guarantee).
- Phase 43 — `trajectory.Trajectory.Serialize` + `trajectory.HandleRegistry` (consumed for checkpoint serialisation + handle re-attachment).
- Phase 42 — `planner.PauseReason` (the `Reason` typedef bridge).

## Risks / open questions

- **Critical-path phase (master plan).** Phase 50 is flagged a highest-risk critical-path phase: "the unified primitive; if it leaks abstractions to planner code, the swappable-planner property regresses." Mitigation: the `Coordinator` lives in `internal/runtime/pauseresume` and never imports the planner package; it consumes `planner.PauseReason` via a *typedef bridge* (`type Reason = planner.PauseReason`), which is a one-directional dependency on a pure enum, not a structural coupling. The import-graph smoke guard pins this.
- **§13 primitive-with-consumer.** Phase 50 ships a primitive without an in-PR consumer. This is permitted because Phase 53 (same wave, Stage 3) is the first `RequestPause`-driven-through-the-Coordinator consumer — see the dedicated section below. The risk is the obligation being forgotten; mitigation is this plan section + the D-067 decisions entry both naming Phase 53 explicitly.
- **Checkpoint envelope vs. Phase 51.** Phase 50 ships a *minimal* checkpoint envelope (`checkpointRecord{FormatVersion int; ...}`); Phase 51 authors the full `format_version: 1` pause-state serialise contract. Risk: Phase 50's envelope shape constrains Phase 51. Mitigation: the envelope carries an explicit `FormatVersion` field (defaulting to `1`) and stores the trajectory bytes opaquely — Phase 51 deepens the envelope's typed fields without a wire break.
- **Postgres integration leg.** The durability integration test's Postgres leg needs a reachable test database; it skips-with-reason when absent (matching `internal/state` driver-test convention). Not a `TODO` skip — a documented environment skip.

## Glossary additions

- **Pause/Resume Coordinator** — the `internal/runtime/pauseresume.Coordinator`; Harbor's ONE pause/resume primitive.
- **Token (pauseresume)** — the opaque, runtime-issued pause handle minted by `Coordinator.Request`.
- **Pause record** — the in-registry (and, when checkpointed, StateStore-persisted) representation of a paused run.
- **Checkpoint store** — an *optional* `state.StateStore` handed to the `Coordinator`; when present, pauses survive Runtime restart.

(All four added to `docs/glossary.md` in this PR.)

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **If this phase builds a reusable artifact: concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`.** The `Coordinator` IS a reusable artifact; `concurrent_test.go` pins N≥100.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists.** Phase 50 consumes `state.StateStore` (Phase 07) + `trajectory` (Phase 43); `test/integration/phase50_durability_test.go` wires real drivers end-to-end.
- [ ] If new vocabulary: glossary updated (4 terms)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (D-067)

## §13 primitive-with-consumer obligation — discharged by Phase 53

CLAUDE.md §13 forbids shipping a primitive without its first consumer **in the same wave**, and names the unified pause/resume primitive explicitly: "Phase 50 (the primitive) cannot ship without at least one planner (or planner upgrade) emitting `RequestPause` for a real reason … in the same wave."

**The obligation is tracked, not forgotten.** The Wave 9 coordinator has decided:

- **Phase 53 (steering wiring), same wave, Stage 3, is the first end-to-end `RequestPause`-driven-through-the-Coordinator consumer.** When a planner emits the `RequestPause` `Decision` shape, Phase 53's steering/executor wiring calls `Coordinator.Request`, drives the protocol-level pause event, and resumes via `Coordinator.Resume` on an inbound `RESUME` steering control. That is the real call site that validates this primitive's design.
- **Phase 51 (Stage 2) also consumes Phase 50's surface** — it authors the pause-record *serialise contract* (`format_version: 1`) on top of the `checkpointRecord` envelope Phase 50 ships and the `trajectory.Trajectory.Serialize` bytes Phase 50 checkpoints.
- **`PauseStep` (Phase 48, `internal/planner/deterministic/steps.go`) already emits the `planner.RequestPause` `Decision` shape** — the producer side of the primitive predates Phase 50; Phase 53 closes the loop by wiring that emission *into the Coordinator*.

Phase 50's own tests are not a substitute for the §13 consumer — they are the *direct* exercise of the primitive (round-trip, durability across all three StateStore drivers, `Status`, idempotent/concurrent `Request`/`Resume`). The §13 obligation is satisfied at the wave level by Phase 53, which lands before Wave 9 closes.
