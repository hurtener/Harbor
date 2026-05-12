# Phase 43 — Trajectory + serialise contract (fail-loudly)

## Summary

Close the **fail-loudly** serialisation contract for the planner's `Trajectory` + `ToolContext` (RFC §6.2 + §3.4). Phase 42 shipped the type skeleton with a stub `Serialize` returning `ErrTrajectoryNotImplemented`; Phase 43 lands the real contract in `internal/planner/trajectory/`: a canonical-JSON `Serialize() ([]byte, error)` that returns `(nil, ErrUnserializable{Field: "..."})` on any non-JSON-encodable entry (no silent-drop path), a `ToolContext` split into a JSON-encodable half + an opaque `HandleID` registry (process-local at V1 per RFC §6.3), a `Deserialize([]byte) (*Trajectory, error)` that round-trips byte-stably, a fail-loud `HandleRegistry.Get` that returns `ErrToolContextLost` on missing handles, and a D-025 concurrent-reuse suite plus the brief 02 §6 negative-case adversarial suite (functions, channels, file descriptors, cyclic graphs).

## RFC anchor

- RFC §6.2
- RFC §3.4
- RFC §6.3
- RFC §3.5

## Briefs informing this phase

- brief 02

## Brief findings incorporated

- **brief 02 §4 (Pause-state serialisation: MUST FAIL LOUDLY).** "When a pause record is serialised, `tool_context` is wrapped in `try: json.loads(json.dumps(...)) except (TypeError, ValueError): return None`. **It silently drops non-serialisable tool context on resume.** The trajectory file itself does the same thing on its `serialise()` method." Phase 43 closes this exact bug: `Trajectory.Serialize()` walks the structure reflectively, returns `(nil, ErrUnserializable{Field: "..."})` naming the offending field path on any non-JSON-encodable leaf, and never falls back to a "set to nil" path.
- **brief 02 §4 (`ToolContext` is split — serialisable + handle registry).** "`ToolContext` is split into a **serialisable** half (IDs, configs, plain values) and a **non-serialisable** half (live callbacks, loggers, sockets). The non-serialisable half is registered with the runtime under a handle key; on resume the handle is re-attached from the runtime's live registry by key. If the handle cannot be re-attached, resume FAILS with `ErrToolContextLost{Handle: "..."}` — never silently." Phase 43 ships exactly this shape: `ToolContext.Serializable map[string]any` + `ToolContext.Handles []HandleID`; the `HandleRegistry` is process-local at V1 (RFC §6.3 + brief 02 §4 final paragraph) with `Get(HandleID) (any, error)` returning `ErrToolContextLost` on a missing handle.
- **brief 02 §4 (byte-stable round-trip is a tested invariant).** "The persistence drivers (in-memory / SQLite / Postgres — see brief 03) must round-trip the trajectory byte-for-byte; a conformance test asserts this." Phase 43's `serialize_test.go` ships the round-trip golden assertion: `Serialize → Deserialize → Serialize` produces byte-identical output. Map keys serialise in alphabetical order via `encoding/json`'s default behaviour; struct fields serialise in declaration order. The two are stable across re-encoding when nested `any` values round-trip through `map[string]any`.
- **brief 02 §5 ("Sharp edges") + §6 (Tests required).** "Negative case: trajectory with a callback in `ToolContext` must produce `ErrUnserializable` (not silently drop)." Phase 43 ships the adversarial test pack — functions, channels, file-handle-shaped types, cyclic graphs all surface as `ErrUnserializable{Field: "..."}` with field-path tracking via a reflective pre-flight walker. "Attempt resume with non-recoverable `ToolContext` handle; must return `ErrToolContextLost`." Phase 43's `registry_test.go` exercises this path.
- **brief 02 §4 (Handle registry persistence).** "V1: process-local. Resume must run in the same Runtime process. The seam for a distributed handle directory exists (the registry is an interface) but no production driver ships at V1." Phase 43 ships `HandleRegistry` as an interface with one V1 implementation (`processLocal{sync.Map}`); the distributed-handle directory remains a post-V1 RFC concern.

## Findings I'm departing from (if any)

- **Subsystem location.** The master plan's `Subsystem` column for Phase 43 reads `planner/trajectory`. Phase 42's `Trajectory` skeleton currently lives directly in `internal/planner/trajectory.go` (file, not subpackage). Phase 43 moves the load-bearing types — `Trajectory`, `Step`, `ToolContext`, `HandleID`, `HandleRegistry`, `ErrUnserializable`, `ErrToolContextLost` — into the `internal/planner/trajectory/` subpackage where the master plan locates them. The legacy planner-package types become type aliases (`type Trajectory = trajectory.Trajectory`) so `RunContext.Trajectory *Trajectory` keeps compiling without changing existing call sites. **Why:** the master plan's subsystem column is binding; subpackage isolation matches the §4.4 extensibility-seam pattern (the `HandleRegistry` interface lives where its drivers will live); and Phase 42's stub `ErrTrajectoryNotImplemented` is retired (no consumer relied on it outside Phase 42's own test, which Phase 43 updates). Recorded as **D-049**.

## Goals

- Ship `internal/planner/trajectory/` as the canonical home for the load-bearing trajectory types (Trajectory, Step, ToolContext, HandleID, HandleRegistry) + the fail-loudly Serialize / Deserialize contract.
- Implement `Trajectory.Serialize() ([]byte, error)` returning `(nil, ErrUnserializable{Field: "..."})` on any non-JSON-encodable leaf. **No silent-drop path.**
- Implement `Deserialize([]byte) (*Trajectory, error)` such that `Serialize → Deserialize → Serialize` is byte-identical (round-trip invariant).
- Split `ToolContext` into a JSON-encodable `Serializable map[string]any` half and an opaque `Handles []HandleID` half; ship `HandleRegistry` interface + process-local driver (`sync.Map[HandleID]any`) with fail-loud `Get` (`ErrToolContextLost`).
- Wire `internal/planner.Trajectory` / `ToolContext` / `ErrUnserializable` / `ErrToolContextLost` as type aliases / re-exports from the subpackage so existing call sites compile unchanged.
- Retire `ErrTrajectoryNotImplemented` — the stub is replaced by the real contract.
- D-025 concurrent-reuse contract: N≥128 goroutines invoking `Serialize` + `HandleRegistry.Get` against shared instances under `-race`, no races, no leaks, no cross-talk.
- §11 pause/resume serialisation tests are mandatory: build a pause-state-shaped `Trajectory` whose `ToolContext` carries a non-serialisable handle; assert `Serialize` returns `ErrUnserializable{Field: "..."}` not `nil`.
- Negative-case adversarial pack: functions, channels, file-descriptor-shaped values, cyclic graphs each surface as `ErrUnserializable{Field: "..."}` with field-path tracking.
- Resume with a stale `HandleID` returns `ErrToolContextLost`, never `nil`.
- Coverage on `internal/planner/trajectory`: ≥ 90%.

## Non-goals

- No distributed handle-registry driver. V1 ships process-local only; the distributed-handle directory is a post-V1 RFC concern (RFC §6.3 + RFC §12).
- No pause/resume Coordinator. Phase 50 (`pauseresume.Coordinator`) ships the resume primitive; Phase 51 ships the pause-record contract that consumes this phase's `Trajectory.Serialize`. Phase 43 ships the data-plane contract; Phase 50/51 ship the control plane.
- No Trajectory compression / summariser. Phase 46 wires the summariser; Phase 43 ships the type slot (`Summary *TrajectorySummary`).
- No runtime executor for Decisions. The runtime engine planner-step phase (later wave) consumes `Trajectory.Steps` end-to-end.
- No StateStore integration. The StateStore (Phase 07) consumes opaque bytes; Phase 43 ships the bytes-producer; the wire-up between trajectory-Serialize and StateStore.Save lands at Phase 51's pause-record contract.
- No Protocol surface. Trajectory is a runtime-internal type; the Protocol's `state.list_trajectories` projection (later phase) consumes a redacted view.
- No fuzzing — happy-path / round-trip / adversarial / concurrent are the gate.

## Acceptance criteria

- [ ] `internal/planner/trajectory/trajectory.go` defines `Trajectory`, `Step`, `Summary`, `Source`, `SteeringInjection`, `BackgroundResult`, `BackgroundMemberOutcome`, `ResumeHint`, `FailureRecord`, `StreamChunk` with explicit JSON struct tags.
- [ ] `internal/planner/trajectory/toolcontext.go` defines `ToolContext{Serializable map[string]any; Handles []HandleID}` and `HandleID` (opaque string type).
- [ ] `internal/planner/trajectory/registry.go` defines the `HandleRegistry` interface + `NewProcessLocalRegistry()` constructor backed by `sync.Map`. `Get(HandleID) (any, error)` returns `(nil, ErrToolContextLost{Handle: id})` on miss — never `(nil, nil)`.
- [ ] `internal/planner/trajectory/errors.go` defines: `ErrUnserializable` (struct with `Field string`, satisfies `error`; sentinel for `errors.As`), `ErrToolContextLost` (struct with `Handle HandleID`; sentinel for `errors.As`). Both have stable `Error()` messages naming the offending field/handle.
- [ ] `Trajectory.Serialize() ([]byte, error)` returns canonical JSON bytes on success. On any non-JSON-encodable leaf, returns `(nil, ErrUnserializable{Field: "<path>"})`. The walker tracks the field path (`"Trajectory.Steps[3].Observation"`) so the error message is actionable.
- [ ] `Deserialize([]byte) (*Trajectory, error)` produces a `*Trajectory` such that `Serialize → Deserialize → Serialize` is byte-identical. Golden bytes asserted in test.
- [ ] `internal/planner/trajectory/trajectory_test.go` covers happy-path Serialize / Deserialize round-trip + the byte-stable golden assertion.
- [ ] `internal/planner/trajectory/serialize_negative_test.go` covers the adversarial pack: a `Trajectory` whose `LLMContext` contains a `func()`, a `chan int`, a non-encodable `*os.File`-shaped value, and a cyclic `map[string]any` each surface as `ErrUnserializable{Field: "..."}`. Field-path assertions are exact strings.
- [ ] `internal/planner/trajectory/toolcontext_test.go` covers: a pause-state-shaped trajectory whose `ToolContext.Serializable` contains a non-encodable leaf returns `ErrUnserializable`; a `ToolContext.Handles` slice round-trips through `Serialize → Deserialize` byte-stably (handle IDs are JSON strings).
- [ ] `internal/planner/trajectory/registry_test.go` covers: `Set/Get` round-trip; `Get` of an unset handle returns `ErrToolContextLost{Handle: ...}` not `nil`; `Delete` removes the handle; cross-handle isolation (setting handle A does not surface under handle B).
- [ ] `internal/planner/trajectory/concurrent_test.go` ships the D-025 concurrent-reuse test: N=128 goroutines invoking `Serialize` against a shared `Trajectory` AND `HandleRegistry.Get/Set/Delete` against a shared registry, under `-race`. Asserts no races, no leak (baseline-restored `runtime.NumGoroutine`), no cross-talk (each goroutine recovers its own handle).
- [ ] `internal/planner/trajectory.go` (the legacy planner-package file) becomes type aliases re-exporting from the subpackage: `type Trajectory = trajectory.Trajectory`, `type Step = trajectory.Step`, `type ToolContext = trajectory.ToolContext`, `type HandleID = trajectory.HandleID`, `var ErrUnserializable = trajectory.ErrUnserializableSentinel`, `var ErrToolContextLost = trajectory.ErrToolContextLostSentinel`. Existing planner-package consumers compile unchanged.
- [ ] `internal/planner/errors.go` removes `ErrTrajectoryNotImplemented` (no consumer outside Phase 42's own test, which is updated in this PR).
- [ ] `internal/planner/planner_test.go` `TestTrajectory_SerializeFailsLoudly` is updated to assert the real contract: a Trajectory with a function in `LLMContext` returns `ErrUnserializable` via `errors.As`.
- [ ] `scripts/smoke/phase-43.sh` exists, executable, runs `go test -race ./internal/planner/trajectory/...` and asserts `ErrUnserializable` / `ErrToolContextLost` are registered sentinel errors via `go doc`.
- [ ] `docs/decisions.md` D-049 records the load-bearing calls (process-local handle registry at V1; canonical JSON ordering; subpackage relocation per the master plan; Phase 42 stub retirement).
- [ ] `docs/glossary.md` gains entries for `HandleID` and updates the existing `Trajectory` / `ToolContext` / `ErrUnserializable` / `ErrToolContextLost` entries to point at Phase 43 as the closing phase.
- [ ] `docs/plans/README.md` Phase 43 row flips to `Shipped`.
- [ ] `README.md` Status table updated.
- [ ] Coverage on `internal/planner/trajectory`: ≥ 90%.

## Files added or changed

- `internal/planner/trajectory/trajectory.go` (new) — Trajectory + Step + nested types with JSON tags.
- `internal/planner/trajectory/toolcontext.go` (new) — ToolContext split + HandleID.
- `internal/planner/trajectory/registry.go` (new) — HandleRegistry interface + process-local driver.
- `internal/planner/trajectory/errors.go` (new) — ErrUnserializable + ErrToolContextLost sentinel struct types.
- `internal/planner/trajectory/serialize.go` (new) — fail-loudly Serialize + Deserialize + reflective walker.
- `internal/planner/trajectory/trajectory_test.go` (new) — happy-path round-trip + golden bytes.
- `internal/planner/trajectory/serialize_negative_test.go` (new) — adversarial pack.
- `internal/planner/trajectory/toolcontext_test.go` (new) — pause-state shape test.
- `internal/planner/trajectory/registry_test.go` (new) — handle registry tests.
- `internal/planner/trajectory/concurrent_test.go` (new) — D-025 concurrent-reuse test.
- `internal/planner/trajectory.go` (modified) — type aliases re-exporting from subpackage; legacy file becomes a thin re-export.
- `internal/planner/errors.go` (modified) — remove `ErrTrajectoryNotImplemented`.
- `internal/planner/planner_test.go` (modified) — update `TestTrajectory_SerializeFailsLoudly` to assert real fail-loud contract.
- `scripts/smoke/phase-43.sh` (new) — runs `go test -race ./internal/planner/trajectory/...` and asserts the sentinel errors exist via `go doc`.
- `docs/plans/phase-43-trajectory.md` (this file).
- `docs/plans/README.md` (modified — Phase 43 row flips to `Shipped`).
- `docs/decisions.md` (modified — D-049 record).
- `docs/glossary.md` (modified — `HandleID` added; existing entries updated to point at Phase 43).
- `README.md` (modified — Status table).

## Public API surface

```go
package trajectory

import (
    "context"
    "encoding/json"
    "time"

    "github.com/hurtener/Harbor/internal/artifacts"
)

// HandleID is the opaque key for a non-serialisable tool-context handle.
// The HandleRegistry's interface methods accept HandleID; the registry
// stores the underlying value (callbacks, loggers, sockets) by key.
type HandleID string

// ToolContext is the planner-facing tool-handle bundle. The split:
//
//   - Serializable: JSON-encodable values. Persisted across pause/resume
//     via Trajectory.Serialize.
//   - Handles: opaque HandleIDs. The actual values live in the runtime's
//     process-local HandleRegistry; on resume, the runtime re-attaches
//     each handle from the registry by ID.
type ToolContext struct {
    Serializable map[string]any `json:"serializable,omitempty"`
    Handles      []HandleID     `json:"handles,omitempty"`
}

// Trajectory is the append-only execution log a planner sees as the
// run progresses. Serialize is the fail-loudly contract (RFC §6.2 + §3.4):
//
//   Serialize() ([]byte, error)
//
// returns canonical JSON bytes on success; on any non-JSON-encodable
// leaf, returns (nil, ErrUnserializable{Field: "<path>"}) — no silent-
// drop path. Deserialize reverses; round-trip is byte-stable.
type Trajectory struct {
    Query          string                       `json:"query,omitempty"`
    LLMContext     map[string]any               `json:"llm_context,omitempty"`
    ToolContext    ToolContext                  `json:"tool_context"`
    Steps          []Step                       `json:"steps,omitempty"`
    Summary        *Summary                     `json:"summary,omitempty"`
    Sources        []Source                     `json:"sources,omitempty"`
    Artifacts      map[string]artifacts.ArtifactRef `json:"artifacts,omitempty"`
    HintState      map[string]any               `json:"hint_state,omitempty"`
    SteeringInputs []SteeringInjection          `json:"steering_inputs,omitempty"`
    Background     map[string]BackgroundResult  `json:"background,omitempty"`
    ResumeHint     *ResumeHint                  `json:"resume_hint,omitempty"`
}

// Serialize returns the canonical JSON byte representation of the
// trajectory. On ANY non-JSON-encodable leaf, returns
// (nil, ErrUnserializable{Field: "<dotted.path>"}). No silent-drop path.
func (t *Trajectory) Serialize() ([]byte, error)

// Deserialize parses canonical JSON bytes into a Trajectory. The
// round-trip Serialize → Deserialize → Serialize is byte-identical.
func Deserialize(b []byte) (*Trajectory, error)

// ErrUnserializable is raised loudly when Serialize encounters a
// non-JSON-encodable leaf. Use errors.As to extract the Field path.
type ErrUnserializable struct {
    Field string
}

func (e ErrUnserializable) Error() string

// ErrToolContextLost is raised loudly when HandleRegistry.Get sees a
// HandleID that has no live registry mapping (typically: a stale handle
// from a serialised trajectory whose registering process died).
type ErrToolContextLost struct {
    Handle HandleID
}

func (e ErrToolContextLost) Error() string

// HandleRegistry holds the non-serialisable half of ToolContext. V1
// driver is process-local (sync.Map-backed). A distributed driver is
// a post-V1 RFC concern.
type HandleRegistry interface {
    Set(id HandleID, value any)
    Get(id HandleID) (any, error)  // returns ErrToolContextLost on miss
    Delete(id HandleID)
}

// NewProcessLocalRegistry constructs the V1 process-local driver.
func NewProcessLocalRegistry() HandleRegistry
```

The full Step / Summary / Source / SteeringInjection / BackgroundResult / BackgroundMemberOutcome / ResumeHint / FailureRecord / StreamChunk shapes live in `trajectory.go` with explicit JSON tags so Serialize / Deserialize bytes are stable.

The planner-package backward-compatibility aliases:

```go
package planner

import "github.com/hurtener/Harbor/internal/planner/trajectory"

type Trajectory = trajectory.Trajectory
type Step = trajectory.Step
type Summary = trajectory.Summary
type ToolContext = trajectory.ToolContext
type HandleID = trajectory.HandleID
type Source = trajectory.Source
type SteeringInjection = trajectory.SteeringInjection
type BackgroundResult = trajectory.BackgroundResult
type BackgroundMemberOutcome = trajectory.BackgroundMemberOutcome
type ResumeHint = trajectory.ResumeHint
type FailureRecord = trajectory.FailureRecord
type StreamChunk = trajectory.StreamChunk

type ErrUnserializable = trajectory.ErrUnserializable
type ErrToolContextLost = trajectory.ErrToolContextLost
```

## Test plan

- **Unit:** `trajectory_test.go` — happy-path Serialize / Deserialize round-trip with a populated Trajectory; golden-bytes assertion (declarative byte string); empty-trajectory zero-value round-trip. `serialize_negative_test.go` — the adversarial pack: function, channel, file-handle-shaped, cyclic graph; each returns `ErrUnserializable{Field: "..."}` with exact path. `registry_test.go` — Set/Get/Delete; miss surfaces `ErrToolContextLost`; cross-handle isolation. `toolcontext_test.go` — the pause/resume serialisation invariant required by §11: non-serialisable leaf in `ToolContext.Serializable` surfaces `ErrUnserializable`; resume with stale `HandleID` surfaces `ErrToolContextLost`.
- **Integration:** Phase 43 consumes Phase 42 (planner package types) + Phase 07 (StateStore — logical, not structural; the wire-up between `Serialize` bytes and `StateStore.Save` lands at Phase 51's pause-record contract). The subpackage's tests prove the data-plane contract; the control-plane integration test (`Trajectory → StateStore.Save → load → Deserialize`) is Phase 51's PR. Mark N/A here: no behavioural consumption of Phase 07's `StateStore.Save`; the byte-producer / byte-consumer are decoupled by design.
- **Conformance:** N/A at this phase. The conformance pack lives at `internal/planner/conformance/` (Phase 49); the trajectory subpackage's own tests are the gate for the Serialize contract. Phase 51 (`pause-state serialise contract`) declares `Tests. Conformance with phase 43 Trajectory.Serialize` — that conformance step rides Phase 51's PR.
- **Concurrency / leak:** `concurrent_test.go` — N=128 goroutines invoking `Serialize` against a shared `Trajectory` AND `HandleRegistry.Get/Set/Delete` against a shared `HandleRegistry`, under `-race`. Asserts: no races (race detector), no cross-talk (each goroutine's handle round-trips its own value through the registry; no other goroutine's value surfaces), no leak (baseline `runtime.NumGoroutine` restored after the WaitGroup join).

## Smoke script additions

- `scripts/smoke/phase-43.sh` runs `go test -race -count=1 -timeout 120s ./internal/planner/trajectory/...` and asserts via `go doc github.com/hurtener/Harbor/internal/planner/trajectory ErrUnserializable` + `ErrToolContextLost` that the sentinel types are exported and present (a regression check that would catch accidental rename / removal). Phase 43 has no protocol surface; the smoke script's role is "the contract types still exist + tests still pass at the live build."

## Coverage target

- `internal/planner/trajectory`: 90%.

## Dependencies

- Phase 42 (Planner iface + Trajectory skeleton). Phase 43 reuses the type slots Phase 42 declared (Step, Summary, Source, etc.) and lands the real Serialize / Deserialize / handle-registry contract.
- Phase 07 (StateStore). Logical dependency: Phase 51's pause-record wire-up will pipe `Trajectory.Serialize` bytes into `StateStore.Save`. Phase 43 ships the byte-producer; Phase 51 ships the byte-consumer pipeline.

## Risks / open questions

- **Byte-stable round-trip and `any`-valued fields.** Fields like `LLMContext map[string]any` round-trip cleanly when the values are themselves JSON-shaped (maps, slices, strings, numbers, bools, nil). When a caller passes a Go struct as an `any` value, the first Serialize encodes it as a struct (declaration-order keys), but Deserialize produces a `map[string]any` (alphabetical-order keys). Round-trip byte equality is therefore guaranteed when:
  1. The caller passes JSON-tree shapes (`map[string]any` / `[]any` / primitives) into `any`-valued fields, OR
  2. The struct's JSON tags happen to match alphabetical field order.
  Phase 43's golden-bytes test uses the JSON-tree shape (case 1); the runtime executor that builds trajectories at the planner-step phase (later wave) follows the same discipline. **Mitigation:** the godoc on `Trajectory.LLMContext` / `HintState` / `Step.Observation` documents the discipline; the runtime's planner-step builder enforces it via a wire-shape normalisation pass (later phase).
- **Reflective walker performance.** The pre-flight walker re-walks every leaf before the real `json.Marshal`. For typical trajectory sizes (≤ a few KB) this is negligible. For pathologically large trajectories (post-Phase 46 compression failure) it remains O(N) — same complexity as `json.Marshal`. **Mitigation:** the walker short-circuits on the first non-encodable leaf; no second pass on the happy path is needed (the marshalled bytes are produced directly after the walker passes).
- **Process-local handle registry.** V1 ships the process-local driver only. Resume MUST run in the same Runtime process. The HandleRegistry interface admits a distributed driver post-V1, but no production driver ships at V1. **Mitigation:** RFC §6.3 already documents this as the V1 constraint; brief 02 §4 calls it explicitly; Phase 50/51's PR will surface the operator-visible constraint at the pause-coordinator boundary.
- **HandleID collision.** HandleIDs are opaque strings; the runtime that registers handles is responsible for collision-free generation (ULID / UUID v4 are the recommended conventions). The registry does not enforce uniqueness on `Set` — re-registering an existing HandleID overwrites silently per the standard `map` semantics. **Mitigation:** the godoc on `Set` documents this; the runtime planner-step phase (later wave) uses ULIDs, which are collision-free in practice.

## Glossary additions

- **`HandleID`** — opaque string identifier for a non-serialisable tool-context handle. Stored in `ToolContext.Handles`; resolved at runtime via `HandleRegistry.Get`. Missing-handle resume surfaces `ErrToolContextLost`. RFC §6.3, Phase 43.
- (Update existing) **`Trajectory`** — now closes Phase 43's fail-loudly Serialize contract; pre-Phase-43 stub `ErrTrajectoryNotImplemented` is retired.
- (Update existing) **`ToolContext`** — Phase 43 implements the serialisable + handle-registry split.
- (Update existing) **`ErrUnserializable`** — Phase 43 ships the struct with `Field string` and the reflective field-path walker.
- (Update existing) **`ErrToolContextLost`** — Phase 43 ships the struct with `Handle HandleID` and the fail-loud `HandleRegistry.Get` path.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target (90% on `internal/planner/trajectory`)
- [ ] If multi-isolation paths changed: cross-session isolation test passes. N/A — Trajectory and HandleRegistry are per-run; identity flows through the planner's RunContext at runtime, not through Trajectory itself.
- [ ] **Concurrent-reuse test passes** — `concurrent_test.go` ships N=128 goroutines exercising `Serialize` + `HandleRegistry.Get/Set/Delete` against shared instances under `-race`. D-025.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists.** N/A — Phase 43 consumes Phase 42 type slots (no behavioural surface) and is a logical (not structural) dependency of Phase 07's StateStore; the integration wire-up lands at Phase 51.
- [ ] If new vocabulary: glossary updated (yes — `HandleID` + four existing-entry updates)
- [ ] If a brief finding was departed from: D-049 records the subpackage relocation.
