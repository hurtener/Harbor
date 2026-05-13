# Phase 46 — Trajectory compression / summariser

## Summary

Land the runtime-side trajectory summariser invoked when the planner-step's token estimate exceeds `RunContext.Budget.TokenBudget`. The phase ships the `Summariser` interface + `TrajectorySummary` shape in `internal/planner/trajectory/`, a reusable `CompressionRunner` (D-025) that owns the "estimate → invoke summariser → stamp `Trajectory.Summary`" loop, the `Budget.TokenBudget` slot the runner consults, and the in-package ReAct consumer wire-up so the primitive lands with its first end-to-end consumer (CLAUDE.md §13 primitive-with-consumer rule). `defaultBuilder` now skips raw step history when `rc.Trajectory.Summary != nil`, rendering the summary as the trajectory representation — compression is a runtime concern; the planner sees only the compacted view. Fail-loudly: summariser errors propagate verbatim, never silently fall through to raw history.

## RFC anchor

- RFC §6.2
- RFC §3.4

## Briefs informing this phase

- brief 02
- brief 07

## Brief findings incorporated

- **brief 02 §4 (Trajectory compression).** "When `token_budget` is exceeded, the runtime invokes a configurable summariser (a separate, cheaper LLM in the reference) to produce a `TrajectorySummary{Goals, Facts, Pending, LastOutputDigest, Note}`. The compressed digest replaces the raw step history in subsequent prompt builds. Compression is a runtime concern (it operates on the trajectory), not a planner concern; the planner sees the compressed view via `RunContext.Trajectory.Summary`." Phase 46 implements this verbatim: `CompressionRunner.MaybeCompress(ctx, rc, tr) error` checks `len(serialised)/4 + 1` against `rc.Budget.TokenBudget` (chars/4 mirrors `internal/llm/tokens.go`'s estimator — single estimator surface, no parallel implementation per §13), and when the threshold is exceeded calls the configured `Summariser.Summarise(ctx, rc, tr)`, stamping the result on `Trajectory.Summary`. The ReAct prompt builder reads `rc.Trajectory.Summary` and skips raw step history when present — the planner observes only the compacted view.
- **brief 02 §5 ("Pause-state serialisation that MUST FAIL LOUDLY").** "When a pause record is serialised, `tool_context` is wrapped in `try: json.loads(json.dumps(...)) except (TypeError, ValueError): return None`. **It silently drops non-serialisable tool context on resume.** The trajectory file itself does the same thing on its `serialise()` method." Phase 46 inherits the fail-loudly principle for compression: when `Summariser.Summarise` returns a non-nil error, `CompressionRunner.MaybeCompress` propagates the error verbatim — no silent fall-through to raw history. The `trajectory.compressed` event ships only on success; `trajectory.compression_failed` ships on summariser error so observability picks up the failure loudly.
- **brief 02 §6 (NoOp decisions are NOT part of the Planner interface — wait-for-steering and trajectory-summarization are Runtime short-circuits).** Settled in RFC §6.3 — Harbor's `Decision` sum has no `NoOp` variant; trajectory summarisation is a Runtime concern. Phase 46 confirms this: the `CompressionRunner` is invoked by the runtime engine BEFORE `Planner.Next` (the engine wiring lands at Phase 47+'s planner-runtime stitch); the planner never sees a summarisation step in its Decision stream. The ReAct integration test wires the runner manually because no runtime engine exists at Phase 46; this proves the contract works end-to-end before the engine consumes it.
- **brief 02 §5 (D-025 concurrent-reuse contract).** "Concretes use functional options... statefulness keyed only by `RunID` is the pattern." Phase 46 ships the `CompressionRunner` as a reusable artifact: the receiver is read-only after construction (it holds the `Summariser` reference + the `TokenEstimator` callable); per-call state lives entirely in `ctx` + `rc` + the per-call serialised bytes. The N=128 concurrent-reuse test pins the contract under `-race`.
- **brief 07 §5 ("the planner observes prior steps as assistant / user-rendered observations").** Phase 45 ships the step-by-step renderer (assistant + user pair per step). Phase 46 extends this: when `Trajectory.Summary` is non-nil, the builder swaps the per-step loop for a single compacted user block (Goals / Facts / Pending / LastOutputDigest / Note). The Phase 45 `defaultBuilder` already reads `Trajectory.Summary` as an additive block alongside the step history; Phase 46 tightens that to "summary REPLACES step history" — matching brief 02 §4's "compressed digest replaces the raw step history in subsequent prompt builds." The integration test asserts the swap directly: an over-budget trajectory triggers compression, the next `Build` call renders the summary and zero per-step turns.

## Findings I'm departing from (if any)

- **Phase 45's `defaultBuilder` rendered `Trajectory.Summary` ADDITIVELY alongside the step history** (see `internal/planner/react/prompt.go` `buildUserContent` — when `Summary != nil` the builder appends a "Trajectory summary so far" block but still loops through `Steps`). Phase 46 departs from this: when `Trajectory.Summary != nil`, `defaultBuilder` SKIPS the per-step assistant/user pair loop and renders only the summary block as the trajectory representation. **Why:** brief 02 §4 explicitly says "The compressed digest **replaces** the raw step history in subsequent prompt builds"; rendering both would double-count tokens and defeat the compression. The Phase 45 additive shape was a forward-compatibility seam against Phase 46 (the master plan called Phase 46 "compression / summariser" and reserved the field); Phase 46 closes the seam by tightening the rendering rule. Recorded as **D-055**.
- **`TrajectorySummary` vs the existing `Summary` type.** Phase 43's `internal/planner/trajectory/trajectory.go` ships the compaction artefact as a struct named `Summary` (with the exact fields RFC §6.2 lists for `TrajectorySummary`: `Goals`, `Facts`, `Pending`, `LastOutputDigest`, `Note`). RFC §6.2 + the master-plan Phase 46 detail block both refer to the type as `TrajectorySummary`. Phase 46 keeps the struct name `Summary` (it is the consumer-side name inside the trajectory package — `Trajectory.Summary *Summary`) AND introduces `TrajectorySummary = Summary` as an exported type alias so callers outside the package have a non-ambiguous name. The same shape, two names: the alias matches the RFC + master plan vocabulary; the local name matches the Phase 43 declaration. Recorded as **D-055**.

## Goals

- Define `Summariser` interface in `internal/planner/` (alongside `Decision` / `Planner` — the trajectory subpackage cannot import `internal/planner` without an import cycle since `internal/planner` already imports `internal/planner/trajectory` via aliases per D-049): `Summarise(ctx context.Context, rc RunContext, tr *Trajectory) (*TrajectorySummary, error)`. Fail-loudly: a non-nil error propagates; the runner never silently swallows it.
- Export `TrajectorySummary` in `internal/planner/` as a type alias on `trajectory.Summary`: `type TrajectorySummary = trajectory.Summary`. This puts the canonical RFC vocabulary at the planner-package call site without changing the underlying struct shape; `Trajectory.Summary` retains the in-struct field name (the JSON tag is `"summary"` — unchanged from Phase 43, so wire compatibility is preserved).
- Add `Budget.TokenBudget int` field in `internal/planner/planner.go`. Zero means "no token budget enforced" (parity with the existing `HopBudget` / `CostCap` conventions where zero = no cap).
- Ship `internal/planner/compression.go` with:
  - `Summariser` interface with `Summarise(ctx, rc, tr) (*TrajectorySummary, error)`.
  - `TrajectorySummary` type alias on `trajectory.Summary`.
  - `TokenEstimator func(*Trajectory) (int, error)` — pluggable estimator. Default `DefaultTokenEstimator` walks `Trajectory.Serialize()` bytes and returns `len/4 + 1` (mirrors `internal/llm/tokens.go::chars4Estimator`; single estimator surface — D-055).
  - `CompressionRunner` struct with `NewCompressionRunner(summariser Summariser, opts ...CompressionOption) *CompressionRunner` constructor.
  - `(*CompressionRunner).MaybeCompress(ctx, rc, tr) error` — token-estimate → optional summariser invocation. Stamps `tr.Summary` on success; emits `trajectory.compressed` on success, `trajectory.compression_failed` on summariser error.
  - Functional option `WithTokenEstimator(TokenEstimator) CompressionOption` for tests.
- Register `trajectory.compressed` + `trajectory.compression_failed` event types + typed payloads (`SafePayload` — counts + identity, never raw trajectory content) in `internal/planner/events.go`. The emits are the load-bearing fail-loudly surfaces (§13): compression is observable in both success and failure paths.
- **Integrate with ReAct (Phase 45 consumer).** Modify `internal/planner/react/prompt.go::defaultBuilder.Build` so that when `rc.Trajectory.Summary != nil`, the builder SKIPS the per-step assistant/user pair loop and renders the summary as the trajectory representation. The summary block replaces — does not augment — the raw history. This is the consumer the §13 primitive-with-consumer rule requires.
- **Identity-mandatory at the runner boundary** (§6 rule 9 + D-001). `MaybeCompress` rejects calls with a missing identity component; wrapped `llm.ErrIdentityMissing` to match the planner's identity-rejection sentinel.
- **D-025 concurrent-reuse pinned**: N≥128 goroutines invoking `CompressionRunner.MaybeCompress` against one shared instance under `-race` (per-goroutine RunContext + per-goroutine trajectory; assert no races, no context bleed, no cancellation cross-talk, no goroutine leaks).
- **End-to-end integration test** (Phase 45 consumer): in `internal/planner/react/`, build an over-budget trajectory (many large steps), invoke `CompressionRunner.MaybeCompress` to stamp `Trajectory.Summary`, then re-invoke `defaultBuilder.Build` and assert: (a) the returned messages contain the summary block, (b) zero per-step assistant turns survive, (c) the LLM-call count remains correct (no double-call from the compaction).
- Coverage on `internal/planner/trajectory` (incremental Phase 46 surface): ≥ 80% (master-plan target).

## Non-goals

- No runtime-engine wiring of the runner. The engine that actually invokes `CompressionRunner.MaybeCompress` before each `Planner.Next` call lands at the planner-runtime stitch (Phase 47+ / the runtime engine wave). Phase 46 ships the primitive + the in-package ReAct consumer wire-up that proves the contract.
- No LLM-backed default summariser implementation. The `Summariser` interface ships with two test fixtures (`staticSummariser` returning a canned summary; `errSummariser` returning a programmable error). The production LLM-backed summariser (a separate, cheaper LLM per brief 02 §4) lands when the runtime engine consumes the runner — operators wire a `Summariser` that invokes `llm.LLMClient.Complete` with a compaction prompt. Phase 46 ships the seam.
- No `Trajectory.HintState` integration. Future planner concretes that want to track "I last summarised at step N" via `HintState` can do so independently; Phase 46's runner is stateless across calls (per-call inspection of `Trajectory.Summary` + the live step count is enough).
- No `Trajectory.Summary` invalidation policy. A summary stamp survives across planner steps; the planner observes the compacted view until the runtime re-invokes `MaybeCompress`. Tearing down stale summaries when the trajectory grows beyond a second budget threshold is a Phase 47+ concern (the engine that owns the runner invocation also owns the cadence policy).
- No multi-tier compression (summary-of-summaries). Phase 46 produces ONE summary per call; recursive compression is post-V1.
- No StateStore persistence of `Trajectory.Summary`. The summary is part of the trajectory; the pause-record contract (Phase 51) carries the full trajectory bytes through `Serialize`; `Summary` round-trips byte-stably as part of that contract — Phase 43's existing serialise contract covers this without changes.

## Acceptance criteria

- [ ] `internal/planner/compression.go` exists and defines:
  - `Summariser` interface with `Summarise(ctx, rc, tr) (*TrajectorySummary, error)`.
  - `TrajectorySummary` exported as `type TrajectorySummary = Summary` (alias on the existing Phase 43 `Summary` struct — no struct-shape change).
  - `TokenEstimator func(*Trajectory) (int, error)` type.
  - `DefaultTokenEstimator` — uses `Trajectory.Serialize() → len/4 + 1`. Errors from `Serialize` propagate (fail-loudly; matches Phase 43's contract).
  - `CompressionRunner` struct with: `NewCompressionRunner(summariser, opts...) *CompressionRunner`, `MaybeCompress(ctx, rc, tr) error`, internal `estimator` + `summariser` fields immutable after construction.
  - `CompressionOption` type + `WithTokenEstimator(TokenEstimator) CompressionOption`.
- [ ] `MaybeCompress` flow (in order):
  1. Honour `ctx.Err()` at entry; return verbatim if cancelled.
  2. Reject missing identity (wrapped `llm.ErrIdentityMissing`).
  3. Defensive nil guards: nil `tr` returns wrapped `ErrNilTrajectory`; nil `summariser` panics at construction (composition error caught at boot — matches `react.New(nil)` shape).
  4. Already-compressed short-circuit: when `tr.Summary != nil`, return `nil` without calling the summariser (idempotent; the engine MAY call multiple times per step boundary).
  5. Compute the token estimate via the configured `TokenEstimator`. Estimator errors propagate verbatim (with the `trajectory.compression_failed` emit).
  6. Budget check: when `rc.Budget.TokenBudget <= 0`, return `nil` (no budget enforced).
  7. When `estimate <= rc.Budget.TokenBudget`, return `nil` (below threshold).
  8. Invoke `summariser.Summarise(ctx, rc, tr)`. A non-nil error → emit `trajectory.compression_failed` with the identity + the estimator's count + the error code, return the error wrapped with context (fail-loudly per §13). A nil `*TrajectorySummary` with nil error → wrapped `ErrEmptySummary` (the summariser's contract is fail-loudly; producing nil is a bug, not a recovery state).
  9. Stamp `tr.Summary = result` and emit `trajectory.compressed` carrying the identity + before/after step count + token estimate.
  10. Return `nil`.
- [ ] `internal/planner/events.go` registers two new event types:
  - `EventTypeTrajectoryCompressed events.EventType = "trajectory.compressed"` + `TrajectoryCompressedPayload` (`SafePayload`).
  - `EventTypeTrajectoryCompressionFailed events.EventType = "trajectory.compression_failed"` + `TrajectoryCompressionFailedPayload` (`SafePayload`).
  - Both payloads carry the run's identity quadruple + the token estimate + the step count; the failure payload carries the error code + truncated message (no raw trajectory content).
- [ ] `internal/planner/planner.go`'s `Budget` struct gains `TokenBudget int` (zero = no cap; semantics match `HopBudget` / `CostCap`). godoc on the field cites RFC §6.2 + brief 02 §4 + Phase 46.
- [ ] `internal/planner/react/prompt.go::defaultBuilder.Build` updates: when `rc.Trajectory != nil && rc.Trajectory.Summary != nil`, the builder skips the per-step assistant/user pair loop. The summary block is rendered as part of the user content. Background-task outcomes still surface as a final user turn (the renderer's existing seam for D-032 push-wake outcomes — unchanged). The per-step loop only fires when `Summary == nil`.
- [ ] `internal/planner/compression_test.go` covers:
  - Happy path: under-budget trajectory → `MaybeCompress` returns nil; `tr.Summary` stays nil; no emit.
  - Over-budget happy path: over-threshold trajectory → summariser invoked once; `tr.Summary` stamped; `trajectory.compressed` event emitted.
  - Idempotency: when `tr.Summary != nil`, `MaybeCompress` returns nil without invoking the summariser.
  - Identity-mandatory: missing tenant / user / session / run → wrapped `llm.ErrIdentityMissing`.
  - Ctx cancellation: pre-cancelled ctx returns `ctx.Err()` before the estimator runs.
  - Fail-loudly: summariser returns error → `trajectory.compression_failed` event emitted; error propagates wrapped.
  - Empty-summary guard: summariser returns `(nil, nil)` → wrapped `ErrEmptySummary`.
  - Token-budget bypass: `rc.Budget.TokenBudget == 0` → `MaybeCompress` returns nil regardless of trajectory size.
  - Estimator error: when `DefaultTokenEstimator`'s `Serialize` call returns an `ErrUnserializable`, the error propagates (Phase 43 contract).
  - `NewCompressionRunner(nil)` panics.
- [ ] `internal/planner/compression_concurrent_test.go` ships the D-025 N=128 stress: shared `CompressionRunner`, per-goroutine `RunContext` + `Trajectory`, asserts no races / no context bleed (each goroutine recovers its own stamped summary) / no cancellation cross-talk (cancel one ctx, siblings complete) / no goroutine leak (baseline `runtime.NumGoroutine` restored).
- [ ] `internal/planner/react/prompt_test.go` extends with `TestDefaultBuilder_WithSummary_SkipsStepHistory` — a trajectory with N steps + non-nil `Summary` → the built `CompleteRequest.Messages` contains the summary in the user block and ZERO assistant turns from the step loop.
- [ ] `internal/planner/react/compression_integration_test.go` (NEW) ships the end-to-end ReAct + compression consumer test:
  - Build an over-budget trajectory (many large steps; large `LLMContext`).
  - Invoke `CompressionRunner.MaybeCompress` with a `staticSummariser` fixture; assert `tr.Summary` is stamped + the compressed event fires on the bus.
  - Re-invoke `defaultBuilder.Build` with the now-summarised trajectory; assert: (i) the system + user blocks are present, (ii) the user block contains the summary text, (iii) zero per-step assistant turns.
  - Run a follow-up `ReActPlanner.Next` with a scripted `_finish` LLM response; assert the planner returns `Finish{Goal}` and the LLM was called exactly once (the compaction did not double-call the LLM).
- [ ] `scripts/smoke/phase-46.sh` exists, executable, runs `go test -race -count=1 -timeout 180s ./internal/planner/trajectory/... ./internal/planner/react/...`, asserts `Summariser` / `TrajectorySummary` / `CompressionRunner` / `EventTypeTrajectoryCompressed` / `EventTypeTrajectoryCompressionFailed` are exported via `go doc` + grep, asserts no `internal/runtime/...` import drift in the trajectory or react packages (re-asserts the Phase 42 import-graph contract).
- [ ] `docs/decisions.md` D-055 records: (a) compression-replaces-step-history rendering rule (departure from Phase 45's additive shape); (b) `TrajectorySummary` alias on existing `Summary` struct; (c) chars/4 token estimator (single estimator surface — no parallel implementation of the `internal/llm/tokens.go` estimator); (d) `Summariser` interface lives in `internal/planner/trajectory/` (sibling to `HandleRegistry`); (e) fail-loudly contract on the summariser boundary; (f) idempotency short-circuit when `Summary != nil`.
- [ ] `docs/glossary.md` gains entries for `Summariser`, `TrajectorySummary`, `Compression budget`, `CompressionRunner`, `trajectory.compressed`, `trajectory.compression_failed`.
- [ ] `docs/plans/README.md` Phase 46 row flips `Pending` → `Shipped`.
- [ ] `README.md` Status table gains a Phase 46 row.
- [ ] Coverage on `internal/planner/trajectory`: ≥ 80% (incremental — Phase 43 maintains ≥ 90% on its existing surface).

## Files added or changed

- `internal/planner/compression.go` (new) — `Summariser` interface, `TrajectorySummary` alias, `TokenEstimator`, `CompressionRunner`, `CompressionOption`, sentinel errors (`ErrNilTrajectory`, `ErrEmptySummary`).
- `internal/planner/compression_test.go` (new) — unit tests (happy / over-budget / idempotency / identity / cancel / fail-loud / empty-summary / budget-bypass / estimator-error / nil-summariser-panics).
- `internal/planner/compression_concurrent_test.go` (new) — D-025 N=128 stress.
- `internal/planner/events.go` (modified) — register `EventTypeTrajectoryCompressed` + `EventTypeTrajectoryCompressionFailed`; ship `TrajectoryCompressedPayload` + `TrajectoryCompressionFailedPayload`.
- `internal/planner/events_test.go` (modified) — assert the new types registered + payloads are `SafePayload`.
- `internal/planner/planner.go` (modified) — `Budget.TokenBudget int` field with godoc.
- `internal/planner/react/prompt.go` (modified) — `defaultBuilder.Build` swaps step-history loop for summary-only rendering when `Summary != nil`.
- `internal/planner/react/prompt_test.go` (modified) — `TestDefaultBuilder_WithSummary_SkipsStepHistory` + companion assertion that summary-absent path is unchanged.
- `internal/planner/react/compression_integration_test.go` (new) — end-to-end ReAct + compression consumer; the §13 primitive-with-consumer-rule gate.
- `scripts/smoke/phase-46.sh` (new) — assertions per "Smoke script additions".
- `docs/plans/phase-46-trajectory-summariser.md` (this file).
- `docs/plans/README.md` (modified) — Phase 46 row → `Shipped`.
- `docs/decisions.md` (modified) — D-055 entry.
- `docs/glossary.md` (modified) — new vocabulary entries.
- `README.md` (modified) — Phase 46 status row.

## Public API surface

```go
package planner

import (
    "context"

    "github.com/hurtener/Harbor/internal/planner/trajectory"
)

// TrajectorySummary is the canonical name for the trajectory's
// compaction artefact. Aliased onto the Phase 43 [trajectory.Summary]
// struct: same shape, the alias matches RFC §6.2 + the master-plan
// Phase 46 vocabulary. D-055.
type TrajectorySummary = trajectory.Summary

// Trajectory is the planner-package alias on [trajectory.Trajectory]
// (introduced in Phase 43 — D-049). Phase 46 uses the alias for the
// Summariser interface signature.
// (Already exists from Phase 43 — referenced for clarity.)

// Summariser is the runtime-side interface a configured compaction
// driver implements. The runner calls Summarise when the trajectory's
// token estimate exceeds [Budget.TokenBudget]. Fail-loudly: an error
// from Summarise propagates verbatim through
// [CompressionRunner.MaybeCompress] — no silent fall-through.
//
// Implementations MUST be safe for concurrent use across runs (the
// runner is a reusable artifact per D-025; the summariser is called
// under the run's ctx).
type Summariser interface {
    Summarise(ctx context.Context, rc RunContext, tr *Trajectory) (*TrajectorySummary, error)
}

// TokenEstimator is the runner's pluggable token-count function. The
// default walks Trajectory.Serialize bytes and returns len/4 + 1 —
// mirroring internal/llm/tokens.go's chars4Estimator (D-055).
type TokenEstimator func(tr *Trajectory) (int, error)

// DefaultTokenEstimator is the chars/4 estimator backed by
// Trajectory.Serialize.
func DefaultTokenEstimator(tr *Trajectory) (int, error)

// CompressionRunner drives the "estimate → optional summariser → stamp
// Trajectory.Summary" loop. Reusable artifact (D-025).
type CompressionRunner struct{ /* unexported fields */ }

// CompressionOption configures a [CompressionRunner] at construction.
type CompressionOption func(*CompressionRunner)

// WithTokenEstimator overrides [DefaultTokenEstimator].
func WithTokenEstimator(est TokenEstimator) CompressionOption

// NewCompressionRunner constructs a runner. nil summariser panics —
// composition error caught at boot.
func NewCompressionRunner(summariser Summariser, opts ...CompressionOption) *CompressionRunner

// MaybeCompress is the entrypoint. See phase-46 plan for the flow.
func (r *CompressionRunner) MaybeCompress(ctx context.Context, rc RunContext, tr *Trajectory) error

// Sentinel errors. Use errors.Is.
var (
    ErrNilTrajectory = errors.New("planner: compression refuses nil trajectory")
    ErrEmptySummary  = errors.New("planner: summariser returned (nil, nil) — contract violation")
)
```

```go
package planner

// Phase 46 additions to events.go.
const (
    EventTypeTrajectoryCompressed        events.EventType = "trajectory.compressed"
    EventTypeTrajectoryCompressionFailed events.EventType = "trajectory.compression_failed"
)

// TrajectoryCompressedPayload — SafePayload.
type TrajectoryCompressedPayload struct {
    events.SafeSealed
    Identity      identity.Quadruple
    StepsBefore   int
    StepsAfter    int
    TokenEstimate int
    OccurredAt    time.Time
}

// TrajectoryCompressionFailedPayload — SafePayload.
type TrajectoryCompressionFailedPayload struct {
    events.SafeSealed
    Identity      identity.Quadruple
    StepsObserved int
    TokenEstimate int
    ErrorCode     string  // "summariser_error" / "empty_summary" / "estimator_error"
    ErrorMessage  string  // truncated to 256 chars
    OccurredAt    time.Time
}

// Budget gains TokenBudget.
type Budget struct {
    Deadline    time.Time
    HopBudget   int
    CostCap     int64
    CostSpent   int64
    TokenBudget int  // Phase 46 — RFC §6.2, brief 02 §4. Zero = no cap.
}
```

## Test plan

- **Unit:**
  - `compression_test.go::TestMaybeCompress_UnderBudget_NoSummariser` — under-budget trajectory; summariser not called; `tr.Summary` stays nil; no emit.
  - `compression_test.go::TestMaybeCompress_OverBudget_StampsSummary` — over-budget; summariser called exactly once; `tr.Summary` stamped; `trajectory.compressed` event observed.
  - `compression_test.go::TestMaybeCompress_AlreadyCompressed_Idempotent` — pre-stamped `tr.Summary`; runner short-circuits; summariser not called.
  - `compression_test.go::TestMaybeCompress_RejectsMissingIdentity` — partial quadruple → wrapped `llm.ErrIdentityMissing`.
  - `compression_test.go::TestMaybeCompress_HonoursCtxCancel` — pre-cancelled ctx → `ctx.Err()` before estimator.
  - `compression_test.go::TestMaybeCompress_FailLoudOnSummariserError` — `errSummariser` returns error → `trajectory.compression_failed` emit + error propagates wrapped.
  - `compression_test.go::TestMaybeCompress_FailLoudOnEmptySummary` — summariser returns `(nil, nil)` → wrapped `ErrEmptySummary`.
  - `compression_test.go::TestMaybeCompress_ZeroBudget_NoOp` — `Budget.TokenBudget == 0` → runner returns nil regardless of size.
  - `compression_test.go::TestMaybeCompress_NilTrajectory_FailLoud` — nil `tr` → wrapped `ErrNilTrajectory`.
  - `compression_test.go::TestNewCompressionRunner_PanicsOnNilSummariser` — `NewCompressionRunner(nil)` panics.
  - `compression_test.go::TestDefaultTokenEstimator_PropagatesSerializeError` — Trajectory with a func in LLMContext → estimator returns `ErrUnserializable`.
  - `compression_test.go::TestNewCompressionRunner_AppliesDefaults` — zero options → `DefaultTokenEstimator`.
- **Integration:**
  - `compression_integration_test.go` (in `internal/planner/react/`) wires real `events.EventBus` (inmem) + a real `CompressionRunner` + a `staticSummariser` + a scripted `llm.LLMClient`. Three scenarios:
    1. **Over-budget happy path → ReAct sees the compacted view.** Build a fat trajectory; `MaybeCompress` stamps `Summary`; `defaultBuilder.Build` renders the summary block + zero per-step turns; the scripted `_finish` response makes `Next` return `Finish{Goal}`; LLM called exactly once (the planner's call; the compaction does not consume the scripted client).
    2. **Compression failure surfaces on the bus.** `errSummariser` returns an error; `MaybeCompress` returns the wrapped error; the bus observes `trajectory.compression_failed` with the run's identity.
    3. **Under-budget → no compression, no emit; ReAct renders raw history.** Builds a small trajectory; the runner short-circuits; `defaultBuilder.Build` still renders the per-step pairs.
- **Conformance:** Phase 46 does not extend the planner conformance pack (Phase 49 owns the scenario surface). The compression primitive is runtime-side; the planner conformance pack tests planner behaviour against a stable RunContext, not the runtime's compression cadence. Mark N/A here.
- **Concurrency / leak:** `compression_concurrent_test.go` ships the D-025 N=128 stress. Per-goroutine identity + per-goroutine trajectory; shared `*CompressionRunner` with a `countingSummariser` that records per-call goroutine IDs in the produced summary's `Note` field. Asserts: no races, no context bleed (each goroutine's stamped summary carries its own goroutine ID via the `Note`), no cancellation cross-talk (pre-cancelled ctxes on i%5==0 return `ctx.Err()` without affecting siblings), no goroutine leak (baseline `runtime.NumGoroutine` restored within 500ms of WaitGroup join).

## Smoke script additions

`scripts/smoke/phase-46.sh`:

- Run `go test -race -count=1 -timeout 180s ./internal/planner/trajectory/...` → OK on pass.
- Run `go test -race -count=1 -timeout 180s ./internal/planner/react/...` → OK on pass (re-asserts the consumer wire-up did not regress Phase 45's tests).
- `go doc` assertions: `Summariser`, `TrajectorySummary`, `CompressionRunner`, `DefaultTokenEstimator`, `WithTokenEstimator`, `NewCompressionRunner` exported from `internal/planner/trajectory`.
- Grep assertion: `EventTypeTrajectoryCompressed` + `EventTypeTrajectoryCompressionFailed` present in `internal/planner/events.go`.
- Grep assertion: `TokenBudget` field present in `internal/planner/planner.go` on the `Budget` struct.
- §13 import-graph guard: no `internal/runtime/...` imports in `internal/planner/trajectory/` or `internal/planner/react/`.
- Final `skip "phase 46: compression primitive has no Protocol surface; the runtime-engine invocation lands at Phase 47+"`.

## Coverage target

- `internal/planner/trajectory`: ≥ 80% (incremental Phase 46 surface; Phase 43 maintains ≥ 90% on its existing surface).
- `internal/planner/react`: ≥ 85% maintained (no coverage regression on the Phase 45 surface).

## Dependencies

- 43 (Trajectory + fail-loudly Serialize — the runner consumes `Trajectory.Serialize` for the default token estimator; the `Trajectory.Summary` field exists from Phase 43).
- 32 (LLM client core — the runner's identity-mandatory guard surfaces wrapped `llm.ErrIdentityMissing`; the production summariser binds an LLM client; Phase 46 ships the seam).
- 45 (Reference ReAct planner — the consumer the §13 primitive-with-consumer rule requires; the integration test in `internal/planner/react/` is the gate).

## Risks / open questions

- **The runner's idempotency short-circuit on `tr.Summary != nil` means a second over-budget growth past the first compression is silently dropped until the engine clears the field.** Phase 46 ships this as the V1 contract; the engine that owns the cadence policy (Phase 47+) is the layer that decides when to re-summarise. The unit test `TestMaybeCompress_AlreadyCompressed_Idempotent` pins the current behaviour; the engine wire-up phase will extend with a re-compaction trigger.
- **The chars/4 estimator under-counts multimodal content.** The `internal/llm/tokens.go` estimator adds 256 tokens per non-text part; Phase 46's `DefaultTokenEstimator` walks `Serialize` bytes and so does NOT count multimodal `Trajectory.LLMContext` parts at 256 tokens each. **Mitigation:** trajectories don't typically carry multimodal parts directly in `LLMContext` — the heavy content is upstream (the LLM-edge safety pass per D-026 rewrites oversize content to `ArtifactRef` before it reaches the trajectory). The Phase 46 estimator counts the artifact-ref envelope, which is byte-stable and < 200 chars. A future estimator that walks the trajectory structurally (re-using the LLM-edge tokeniser) is a Phase 47+ refinement; the seam (the `TokenEstimator` functional option) is already in place.
- **The `TrajectoryCompressedPayload.StepsAfter` is always equal to `StepsBefore` in Phase 46** because the runner stamps a summary but does NOT truncate the `Steps` slice. The planner consumes the compacted view through the prompt builder's summary-only rendering — the underlying `Trajectory.Steps` slice is preserved for observability. A future phase MAY truncate the slice in-place; the payload's `StepsAfter` field exists so the schema is forward-compatible without a payload-version bump.
- **The summariser's error code in `TrajectoryCompressionFailedPayload.ErrorCode` is heuristic.** The runner classifies into three buckets (`summariser_error` / `empty_summary` / `estimator_error`). A future fault taxonomy from a production LLM-backed summariser could surface richer codes; the payload's `ErrorMessage` field carries the truncated original. The bucket name is the load-bearing observability surface for now.
- **The Phase 45 `defaultBuilder` rendering change is a behavioural departure** (additive → replacing). Existing Phase 45 tests that pass a non-nil `Summary` with non-empty `Steps` need to be checked: the existing `prompt_test.go` tests either omit `Summary` or assert it in isolation; the Phase 46 rendering change does NOT break them, but the PR's diff review is the gate. Recorded as part of D-055.

## Glossary additions

- **`Summariser`** — runtime-side interface (`internal/planner/trajectory.Summariser`) for the trajectory-compaction driver. Implementations produce a `TrajectorySummary` from a `Trajectory` + `RunContext`. Fail-loudly: errors propagate verbatim through `CompressionRunner.MaybeCompress`. Production wiring binds an LLM client + a compaction prompt; Phase 46 ships the seam + test fixtures. RFC §6.2, brief 02 §4, D-055.
- **`TrajectorySummary`** — the compaction artefact stamped on `Trajectory.Summary` when the runtime invokes the summariser. Five fields per RFC §6.2: `Goals`, `Facts`, `Pending`, `LastOutputDigest`, `Note`. Phase 46 exports it as a type alias for the Phase 43 in-package `Summary` struct so the name matches the RFC + master plan vocabulary. RFC §6.2, D-055.
- **`Compression budget`** — `Budget.TokenBudget int` (Phase 46). The token-estimate threshold above which the runtime invokes the trajectory summariser. Zero means no compression. Estimated via the `TokenEstimator` (default: chars/4 via `Trajectory.Serialize`). RFC §6.2, brief 02 §4, D-055.
- **`CompressionRunner`** — runtime-side reusable artifact (D-025) at `internal/planner/trajectory.CompressionRunner` that owns the "estimate → optional summariser invocation → stamp Trajectory.Summary" loop. Idempotent on `Summary != nil`. RFC §6.2, D-055.
- **`trajectory.compressed`** — event emitted by `CompressionRunner.MaybeCompress` on successful summary stamping. `TrajectoryCompressedPayload` (SafePayload) carries identity + step count + token estimate. RFC §6.2, D-055.
- **`trajectory.compression_failed`** — event emitted by `CompressionRunner.MaybeCompress` when the summariser returns an error or empty summary, or when the estimator errors. `TrajectoryCompressionFailedPayload` (SafePayload) carries identity + error code + truncated message. The fail-loudly observability surface (§13). D-055.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — the D-025 stress + per-goroutine identity quadruple pinning in `compression_concurrent_test.go` covers this; cross-session bleed would surface in the per-goroutine summary stamp inspection.
- [ ] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. The `CompressionRunner` IS a reusable artifact; `compression_concurrent_test.go` ships the N=128 stress.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md §17. Phase 46 consumes Phase 43 (`Trajectory.Serialize`) + Phase 32 (`llm.ErrIdentityMissing`) + Phase 45 (`ReActPlanner` + `defaultBuilder`); `compression_integration_test.go` wires a real `events.EventBus` (inmem) + a real `CompressionRunner` + the production `defaultBuilder` end-to-end; the failure-mode scenario uses `errSummariser` and asserts the bus observes the failure event.
- [ ] If new vocabulary: glossary updated — YES (6 new entries).
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — D-055 (the Phase 45 additive-rendering departure).
