# Phase 24 — Memory strategies (`truncation`, `rolling_summary`)

## Summary

Land the two remaining memory strategies on top of Phase 23's `MemoryStore` surface: `truncation` (synchronous recent-window + budget enforcement) and `rolling_summary` (background-summarised long-term context with a `healthy → retry → degraded → recovering` health FSM). Ship the injectable `Summarizer` interface the LLM-client (Phase 32+) will satisfy and drive the strategies with a stub at this phase. Add the `memory.health_changed` audit event so health transitions are observable on the bus. The strategies are behaviour modes of any `MemoryStore` driver (per AGENTS.md §4.4 binding rule) — the InMem driver gains the new strategies at this phase; Phase 25's SQLite + Postgres drivers will run the same conformance suite verbatim.

## RFC anchor

- RFC §6.6
- RFC §3.5
- RFC §4

## Briefs informing this phase

- brief 04

## Brief findings incorporated

- **brief 04 §4.1 ("Memory strategies").** The brief settles the three-strategy taxonomy and the precise semantics of each: `truncation` is "append turn → keep last `FullZoneTurns` verbatim → enforce `TotalMaxTokens` per `OverflowPolicy`. Synchronous." Phase 24 adopts this verbatim. `rolling_summary` is "append turn → evict older turns into `pending` → schedule background summarization. Summarizer is an injectable async callable: input `{previous_summary, turns}`, output `{summary: string}`. The runtime spawns one task at a time per memory key and lock-protects all state." Phase 24 ships exactly this shape with one in-flight summarisation per memory key, lock-protected via a per-`(identity.Quadruple)` mutex map guarded by the strategy executor's `sync.Mutex`.
- **brief 04 §4.1 ("Health states gate behavior").** "Health states (`healthy → retry → degraded → recovering → healthy`) gate behavior: in `degraded`, the memory falls back to truncation-style and queues a recovery loop bounded by `RecoveryBacklogMax`." Phase 24 implements the FSM as a typed transition table — illegal transitions return `ErrInvalidHealthTransition` so misuse is loud, not silent. Degraded mode falls back to recent-window truncation semantics; recovery loop is bounded by `RecoveryBacklogMax` (default 16; overflow drops oldest and emits `memory.recovery_dropped`, see D-035).
- **brief 04 §4.1 ("Failure semantics").** "Summarizer exceptions never leak. The store falls to `degraded`, drops summarization, keeps the conversation usable from a recent window, and emits a `memory.health_changed` event the Console can render." Phase 24 implements this directly: a Summarizer error trips a counted retry; after `RetryAttempts` consecutive failures the strategy transitions to `HealthDegraded` and emits `memory.health_changed`. The observable health-transition emit is the explicit exception to AGENTS.md §13's "no silent degradation" rule — degraded mode IS the observable failure path.
- **brief 04 §6 ("Cross-session no-leak (race)").** "100 concurrent sessions × random AddTurn / GetLLMContext / Snapshot for 30s under `-race`. Final invariant: every `GetLLMContext` output's identity matches the caller's identity exactly." Phase 24 inherits Phase 23's `Concurrent_AllMethods_NoRace` conformance subtest and extends it to run against both `truncation` and `rolling_summary` strategies — 128 goroutines × all methods × per-`(identity.Quadruple)` key isolation. Per-strategy executors are themselves reusable artifacts (D-025): a single executor is shared across all goroutines and per-key state lives in the lock-guarded map. A second concurrent-reuse test exercises the recovery loop coordinator's bounded-queue invariant under concurrent failure injection.
- **brief 04 §2 ("`Summarizer` callable shape").** Brief 04 doesn't name the Go shape, but the predecessor's hint is "input `{previous_summary, turns}`, output `{summary: string}`." Phase 24 ships `Summarizer.Summarize(ctx context.Context, id identity.Quadruple, req SummarizeRequest) (SummarizeResponse, error)` — context first per AGENTS.md §5, identity explicit so the implementer can scope LLM calls, and a typed request/response pair so the LLM-client integration at Phase 32+ doesn't have to invent a fresh shape. A stub `EchoSummarizer` (returns `previous_summary + "\n" + last-N-turns-joined`) drives the tests; the real LLM-backed implementation lands at Phase 32+.

## Findings I'm departing from (if any)

- **`OverflowPolicy` enum from brief 04 §2 is partially adopted.** Brief 04 declares `truncate_oldest`, `truncate_summary`, and `error`. Phase 24 lands `OverflowDropOldest` only (renamed from `truncate_oldest` for Go-idiomatic naming — the action is "drop", not "truncate", on the buffer). `truncate_summary` requires the summariser inside the truncation path which conflates strategies; `error` is a silent-degradation footgun (an over-budget AddTurn returning `ErrBudgetExceeded` would force every caller to handle the error or silently lose turns). Phase 24's truncation path is unconditionally drop-oldest, matching the simplest brief reading. If future operators demand `error` semantics the enum can grow; right now keeping the surface narrow avoids the silent-degradation pitfall this codebase explicitly closes (AGENTS.md §13). Recorded in D-035.
- **Brief 04 mentions `RetryAttempts`, `RetryBackoffBase`, `DegradedRetryEvery` as `MemoryConfig` fields.** Phase 24 lands `RecoveryBacklogMax` only in `config.MemoryConfig`. The retry / backoff / degraded-retry-cadence knobs are encoded as constants on the `rolling_summary` executor (`defaultRetryAttempts=3`, `defaultRetryBackoffBase=100*time.Millisecond`, `defaultDegradedRetryEvery=10*time.Second`) so an operator who needs to tune them files an issue + RFC PR rather than fighting yaml. This keeps the config surface stable and avoids exposing knobs no one has needed yet. If the LLM-client integration (Phase 32+) surfaces real-world miscalibration, we re-litigate via an RFC PR. Recorded in D-035.

## Goals

- Two operational memory strategies: `truncation` (synchronous recent-window + budget enforcement; drop-oldest on overflow) and `rolling_summary` (background-summarised long-term context with health FSM + bounded recovery loop).
- Injectable `Summarizer` interface — the LLM call lives in Phase 32+; this phase ships the interface + a stub (`EchoSummarizer`) used by tests.
- `Health` FSM with explicit transitions (`healthy ↔ retry ↔ degraded ↔ recovering`) tested as a matrix.
- `memory.health_changed` typed event registered via `events.RegisterEventType`, with a `SafePayload` carrying `(PriorHealth, NewHealth, Reason)`.
- `config.MemoryConfig.RecoveryBacklogMax` added (default 16); validator coverage; example yaml documentation.
- `memory.Open` accepts `truncation` and `rolling_summary` — the `ErrStrategyNotImplemented` rejection from Phase 23's InMem driver is replaced by real executors.
- Conformance suite extended with strategy-matrix subtests; the InMem driver passes the suite against all three strategies. Phase 25's SQLite + Postgres drivers will inherit the same suite verbatim.
- Concurrent-reuse contract (D-025): a single strategy executor is shared across N≥128 concurrent goroutines under `-race`; per-key state is mutex-guarded; recovery-loop coordinator's bounded queue invariant holds under concurrent failure injection.
- Cross-subsystem integration test: real audit + events + state + memory drivers wired end-to-end against the stub `Summarizer`; asserts the `memory.health_changed` transition is observable on the bus when the summariser is forced to fail repeatedly.

## Non-goals

- No LLM-backed Summarizer implementation — Phase 32+ owns the LLM-client wiring. This phase ships only the interface + the test-grade `EchoSummarizer` stub.
- No SQLite / Postgres memory drivers — Phase 25 ships those; they inherit this phase's strategy executors and conformance suite unchanged.
- No `OverflowPolicy = "error"` or `"truncate_summary"` semantics — the truncation path is unconditionally drop-oldest (departure from brief 04 §2, recorded in D-035).
- No episodic memory tier — RFC §11 Q-4; explicit post-V1 follow-up.
- No `IncludeTrajectory` / `TrajectoryDigest` ingestion — Phase 24 round-trips the field through the JSON record but applies no strategy logic to it; planner runtime (Phase 42+) is the producer that will exercise it.
- No `MemoryConfig.RetryAttempts` / `RetryBackoffBase` / `DegradedRetryEvery` knobs — defaults are baked into the rolling-summary executor (departure from brief 04 §2, recorded in D-035).
- No HTTP / Protocol surface — memory is a Go-only surface at Phase 24; the smoke script SKIPs per the §4.1 convention.

## Acceptance criteria

- [ ] `internal/memory/memory.go` declares the `Summarizer` interface, `SummarizeRequest` / `SummarizeResponse` types, the `Health` transition table (`ValidateHealthTransition` + `ErrInvalidHealthTransition`), and the `OverflowPolicy` enum (`OverflowDropOldest` only). Existing types (`Strategy`, `Health`, `ConversationTurn`, `LLMContextPatch`, `Snapshot`) remain stable.
- [ ] `internal/memory/events.go` registers `memory.health_changed` and exports `HealthChangedPayload` (SafePayload). The init() registers BOTH event types — the existing rejection one plus the new transition one.
- [ ] `internal/memory/strategy/` (new package) exposes a `StrategyExecutor` interface with the same seven core methods that `MemoryStore` calls into; concrete executors `Truncation` and `RollingSummary`; a constructor (`New(strategy memory.Strategy, deps Deps) (StrategyExecutor, error)`). The package depends only on `internal/memory`, `internal/state`, `internal/events`, `internal/identity`, `internal/audit` (transitively via events).
- [ ] The `Truncation` executor: `AddTurn` appends + drops-oldest when over `BudgetTokens`; `GetLLMContext` returns the recent-window turns + token estimate; `EstimateTokens` returns the current token count; `Flush` clears the buffer; `Health` always `HealthHealthy`; `Snapshot` / `Restore` round-trips the buffer as JSON through `state.StateStore`.
- [ ] The `RollingSummary` executor: `AddTurn` appends to the recent buffer; when the buffer exceeds `FullZoneTurns` (constant), the overflow turns go into a `pending` queue and a single in-flight summariser task is scheduled (one per memory key, via per-key mutex); on success the summary updates and `pending` clears; on failure the executor increments a retry counter, transitions to `HealthRetry`, retries up to `defaultRetryAttempts` times, then transitions to `HealthDegraded`, emits `memory.health_changed`, queues the failed batch into the recovery backlog (bounded by `RecoveryBacklogMax`), and falls back to truncation semantics for `GetLLMContext`. A periodic recovery loop (cancellable via the executor's lifecycle context, `Close`) attempts to drain the backlog at `defaultDegradedRetryEvery` cadence; on success transitions `HealthDegraded → HealthRecovering → HealthHealthy` (one transition per recovery batch drained); each transition emits `memory.health_changed`.
- [ ] `internal/memory/drivers/inmem/inmem.go` `New` accepts all three strategies; routes `StrategyTruncation` / `StrategyRollingSummary` to the corresponding executor; the previous `ErrStrategyNotImplemented` rejection is removed (the constant remains as the sentinel a Phase 25 driver might emit for a future strategy not yet implemented). The driver delegates every method call to the executor.
- [ ] `config.MemoryConfig.RecoveryBacklogMax int` added with `yaml:"recovery_backlog_max,omitempty"`; default `16`; validator (`recovery_backlog_max >= 0`).
- [ ] `config.MemoryConfig.Strategy` allowlist extended in `validateMemory()` to accept `"truncation"` and `"rolling_summary"` alongside `"none"`.
- [ ] `examples/harbor.yaml` `memory:` block updated — `strategy:` comment lists all three operational strategies; `recovery_backlog_max:` documented.
- [ ] `internal/memory/conformancetest/conformancetest.go` extended with strategy-matrix subtests: `Truncation_*` and `RollingSummary_*` subtests assert the strategy-specific semantics. The existing `Strategy=none` subtests stay green; the suite now exercises the full matrix when the factory wires the relevant strategy.
- [ ] `internal/memory/strategy/strategy_test.go` ships the transition matrix property test (all combinations of `Health` × `Health` — valid transitions return nil, invalid return `ErrInvalidHealthTransition`).
- [ ] `internal/memory/strategy/rolling_summary_test.go` covers the recovery-loop bound (concurrent failure injection floods the backlog beyond `RecoveryBacklogMax`; asserts drop-oldest behaviour + `memory.recovery_dropped` emit).
- [ ] `internal/memory/drivers/inmem/inmem_test.go` invokes the conformance suite against all three strategies via separate `t.Run` blocks.
- [ ] Concurrent-reuse test (D-025): a single `Truncation` executor + a single `RollingSummary` executor each shared across N≥128 goroutines × all methods, exercising different `(identity.Quadruple)` keys; assert no data races, no goroutine leak, no cross-identity bleed (each goroutine's reads return only its own bytes).
- [ ] `test/integration/memory_strategies_test.go` (new) wires real audit + events + state + memory drivers against the stub `EchoSummarizer`. Asserts: (a) `truncation` round-trips a multi-turn conversation, (b) `rolling_summary` health-transitions `healthy → degraded` when a failing summariser is wired in, (c) the `memory.health_changed` event is observable on the bus.
- [ ] Coverage on `internal/memory/strategy` ≥ 85%; coverage on `internal/memory/drivers/inmem` stays ≥ 85% (it gains lines for strategy routing).
- [ ] `make drift-audit` and `make preflight` green at commit time.
- [ ] `scripts/smoke/phase-24.sh` present and executable; SKIPs cleanly (no HTTP surface).
- [ ] `docs/glossary.md` gains entries for `Summarizer`, `OverflowPolicy`, `RecoveryBacklogMax`, `memory.health_changed`, `memory.recovery_dropped`.
- [ ] `docs/decisions.md` gains entry **D-035** documenting (a) the unconditional drop-oldest truncation policy and the rationale for not shipping `error` / `truncate_summary`, (b) the bounded recovery-loop with drop-oldest + `memory.recovery_dropped` emit, and (c) the configuration surface narrowing (only `RecoveryBacklogMax` lands; retry/backoff defaults are constants). (D-035 follows D-034 — persistent memory drivers own `memory_state` tables — in the Wave 7a memory cluster.)
- [ ] `docs/plans/README.md` flips Phase 24 from `Pending` to `Shipped` (the row + the detail block under §24).
- [ ] `README.md` Status table gains a Phase 24 row.

## Files added or changed

- `internal/memory/memory.go` (modified) — add `Summarizer` interface, `SummarizeRequest` / `SummarizeResponse`, `OverflowPolicy` + `OverflowDropOldest`, `ValidateHealthTransition` + `ErrInvalidHealthTransition` + transition table.
- `internal/memory/events.go` (modified) — register `memory.health_changed` + `memory.recovery_dropped`; export `HealthChangedPayload` (SafePayload) + `RecoveryDroppedPayload` (SafePayload).
- `internal/memory/health.go` (new) — health-transition helpers (`EmitHealthChanged(ctx, bus, id, prior, next, reason)`); kept separate from `events.go` to mirror the `reject.go` split.
- `internal/memory/strategy/strategy.go` (new) — `StrategyExecutor` interface + `Deps` struct + `New(strategy, deps)` constructor.
- `internal/memory/strategy/truncation.go` (new) — `truncationExec` concrete.
- `internal/memory/strategy/rolling_summary.go` (new) — `rollingSummaryExec` concrete; per-key mutex map; in-flight summariser scheduling; recovery loop.
- `internal/memory/strategy/echo_summarizer.go` (new) — `EchoSummarizer` test-grade stub. Exported so tests in `internal/memory/...` AND `test/integration/...` reuse it.
- `internal/memory/strategy/strategy_test.go` (new) — transition matrix + constructor + executor route tests.
- `internal/memory/strategy/truncation_test.go` (new) — drop-oldest behaviour; budget enforcement; snapshot/restore round-trip.
- `internal/memory/strategy/rolling_summary_test.go` (new) — happy path; failure-injection → degraded; recovery loop drains; backlog bound + `memory.recovery_dropped`.
- `internal/memory/drivers/inmem/inmem.go` (modified) — accept the new strategies; route through the strategy executor.
- `internal/memory/drivers/inmem/inmem_test.go` (modified) — conformance suite invoked against all three strategies.
- `internal/memory/conformancetest/conformancetest.go` (modified) — add `Truncation_*` + `RollingSummary_*` subtests; factory grows a `Strategy` field on `Harness` so subtests can fork based on strategy.
- `internal/config/config.go` (modified) — `MemoryConfig.RecoveryBacklogMax int`; field doc.
- `internal/config/validate.go` (modified) — extend allowlist; validate `RecoveryBacklogMax >= 0`.
- `internal/config/validate_test.go` (modified) — new field coverage.
- `internal/config/loader.go` (modified) — default `RecoveryBacklogMax = 16` when the section is omitted.
- `examples/harbor.yaml` (modified) — `memory:` block updated.
- `test/integration/memory_strategies_test.go` (new) — cross-subsystem integration test.
- `scripts/smoke/phase-24.sh` (new) — SKIP-only smoke (no HTTP surface).
- `docs/plans/phase-24-memory-strategies.md` (this file).
- `docs/plans/README.md` (modified) — flip row + detail block to `Shipped`.
- `docs/glossary.md` (modified) — new entries.
- `docs/decisions.md` (modified) — append D-035.
- `README.md` (modified) — Phase 24 row.

No new top-level directories — `internal/memory/strategy/` sits under `internal/memory/` which is already enumerated in AGENTS.md §3.

## Public API surface

```go
package memory

// Summarizer is the injectable callable rolling_summary consumes.
// Phase 24 ships only the interface + a test-grade stub
// (`EchoSummarizer`); the real LLM-backed implementation lands at
// Phase 32+.
type Summarizer interface {
    Summarize(ctx context.Context, id identity.Quadruple, req SummarizeRequest) (SummarizeResponse, error)
}

// SummarizeRequest carries the inputs the summariser sees.
type SummarizeRequest struct {
    PreviousSummary string
    Turns           []ConversationTurn
}

// SummarizeResponse is the summariser's output.
type SummarizeResponse struct {
    Summary string
}

// OverflowPolicy is the buffer-overflow action under truncation.
// Phase 24 ships only OverflowDropOldest.
type OverflowPolicy string

const OverflowDropOldest OverflowPolicy = "drop_oldest"

// ValidateHealthTransition returns ErrInvalidHealthTransition when
// the supplied (prior, next) is not a legal transition in the
// `healthy ↔ retry ↔ degraded ↔ recovering` FSM. Used by strategy
// executors to gate transitions and by tests for the matrix.
func ValidateHealthTransition(prior, next Health) error

// ErrInvalidHealthTransition is returned by ValidateHealthTransition.
var ErrInvalidHealthTransition = errors.New("memory: invalid health transition")
```

```go
package memory

// HealthChangedPayload reports a health-state transition. SafePayload
// — bounded enumerable strings.
type HealthChangedPayload struct {
    events.SafeSealed
    PriorHealth Health
    NewHealth   Health
    Reason      string
}

// RecoveryDroppedPayload reports a recovery-backlog overflow drop.
// SafePayload — bounded enumerable strings.
type RecoveryDroppedPayload struct {
    events.SafeSealed
    Reason string
}

// EventTypeMemoryHealthChanged + EventTypeMemoryRecoveryDropped
// registered via events.RegisterEventType in init().
const (
    EventTypeMemoryHealthChanged   events.EventType = "memory.health_changed"
    EventTypeMemoryRecoveryDropped events.EventType = "memory.recovery_dropped"
)
```

```go
package strategy

// StrategyExecutor is the same shape as MemoryStore minus Close —
// the driver owns lifecycle.
type StrategyExecutor interface {
    AddTurn(ctx context.Context, id identity.Quadruple, turn memory.ConversationTurn) error
    GetLLMContext(ctx context.Context, id identity.Quadruple) (memory.LLMContextPatch, error)
    EstimateTokens(ctx context.Context, id identity.Quadruple) (int, error)
    Flush(ctx context.Context, id identity.Quadruple) error
    Health(ctx context.Context, id identity.Quadruple) (memory.Health, error)
    Snapshot(ctx context.Context, id identity.Quadruple) (memory.Snapshot, error)
    Restore(ctx context.Context, id identity.Quadruple, snap memory.Snapshot) error
    // Close releases per-executor resources (recovery loop ticker,
    // in-flight summariser cancellations). Idempotent.
    Close(ctx context.Context) error
}

// Deps carries the runtime dependencies an executor needs.
type Deps struct {
    State        state.StateStore
    Bus          events.EventBus
    Summarizer   memory.Summarizer // required for rolling_summary; ignored for truncation
    BudgetTokens int
    RecoveryBacklogMax int
}

// New constructs the strategy executor for the given strategy.
func New(s memory.Strategy, deps Deps) (StrategyExecutor, error)

// EchoSummarizer is a test-grade Summarizer that concatenates the
// previous summary + last-N-turns (joined). Used by Phase 24 tests
// and by Phase 24's smoke; the LLM-backed implementation lands at
// Phase 32+.
type EchoSummarizer struct{}
```

## Test plan

- **Unit:**
  - `ValidateHealthTransition`: matrix of all `(Health × Health)` pairs; valid transitions return nil; invalid return `ErrInvalidHealthTransition`. Property: every pair listed in the transition table is reachable; no pair outside the table is accepted.
  - `Summarizer` stub (`EchoSummarizer.Summarize`): identity propagation; empty-turns + empty-prior-summary returns empty; turns concat in order.
  - `strategy.New`: routes `StrategyTruncation` to `truncationExec`, `StrategyRollingSummary` to `rollingSummaryExec`; `StrategyNone` returns the existing no-op executor; unknown strategy returns wrapped error.
  - `truncationExec.AddTurn` drops-oldest at `BudgetTokens` boundary; `EstimateTokens` matches actual buffer state.
  - `truncationExec.Snapshot` / `Restore` round-trip a multi-turn buffer byte-stable (JSON form).
  - `rollingSummaryExec.AddTurn` schedules a single in-flight summariser per key; concurrent AddTurns serialize through the per-key mutex.
  - `rollingSummaryExec.Summarize` failure path: forced `errSummarizer{}` returns the error → retry counter increments → after `defaultRetryAttempts` consecutive failures transitions to `HealthDegraded`; `memory.health_changed` event observable.
  - Recovery-loop bound: floods `RecoveryBacklogMax + N` failing summarisations; asserts the backlog stays at `RecoveryBacklogMax`; oldest entries dropped; `memory.recovery_dropped` events count matches `N`.
- **Integration:**
  - `test/integration/memory_strategies_test.go` wires real audit + events + state + memory + the stub `EchoSummarizer`. Asserts: (a) `truncation` round-trips an N-turn conversation through Snapshot/Restore; (b) `rolling_summary` with a forced-failing summariser transitions `healthy → retry → degraded` and emits `memory.health_changed`; (c) when the summariser is restored the recovery loop drains and transitions `degraded → recovering → healthy`.
- **Conformance:**
  - `conformancetest.Run` extended with strategy-aware subtests. The factory's `Harness` grows a `Strategy memory.Strategy` field; subtests fork on strategy. The InMem driver's `*_test.go` invokes the suite three times (one per strategy).
- **Concurrency / leak (D-025):**
  - `Concurrent_AllMethods_NoRace_Truncation` — N≥128 goroutines × all methods against one shared driver under `-race`.
  - `Concurrent_AllMethods_NoRace_RollingSummary` — same shape against the rolling-summary executor.
  - Recovery-loop coordinator's bounded-queue invariant under concurrent failure injection (a separate test exercises the queue without the full driver).

## Smoke script additions

- `scripts/smoke/phase-24.sh` issues `skip "phase 24: memory strategies — Go package only; validated by go test ./internal/memory/..."` and calls `smoke_summary`. Phase 24 has no HTTP / Protocol surface (same as Phase 23); correctness is verified by `go test -race ./internal/memory/...` and the integration test. SKIP increments cleanly under preflight; no FAIL.

## Coverage target

- `internal/memory`: ≥ 85% (new helpers — `ValidateHealthTransition`, `EmitHealthChanged`, `HealthChangedPayload`, `RecoveryDroppedPayload`, `Summarizer` interface).
- `internal/memory/strategy`: ≥ 85% (new package; executors are the load-bearing surface).
- `internal/memory/drivers/inmem`: ≥ 85% (now routes through three strategies).
- `internal/memory/conformancetest`: not gated (the helper's t.Errorf / t.Fatalf only fire when a downstream driver fails).

## Dependencies

- Phase 23 (memory) — `MemoryStore` interface + InMem driver + conformance suite.
- Phase 07 (state) — `state.StateStore` for snapshot/restore persistence.
- Phase 05 (events) — `events.EventBus` for emitting `memory.health_changed` + `memory.recovery_dropped`.
- Phase 03 (audit) — declared logically; SafePayloads skip redaction.

## Risks / open questions

- **Recovery-loop ticker leak under fast Open/Close churn.** The `rolling_summary` executor starts a background goroutine for the recovery loop; Close must cancel it. Test: open → close → assert `runtime.NumGoroutine` returns to baseline within a bounded deadline (precedent: Phase 07 + Phase 23's concurrent-reuse test).
- **Per-key mutex map growth.** With many concurrent identities the per-key mutex map grows unboundedly inside the executor. At Phase 24 this is acceptable for `inmem` driver (in-memory store, process-scoped); Phase 25's persistent drivers should consider eviction (likely tied to session GC). Documented inline; Phase 25 owns the follow-up.
- **`OverflowPolicy` narrowing vs. brief 04 §2.** Documented in "Findings I'm departing from" + D-035. If the LLM-client integration (Phase 32+) surfaces a real-world need for `truncate_summary` (e.g. "always keep a summary line, drop oldest verbatim turns first"), we open an RFC PR to grow the enum; today the simpler shape avoids the silent-degradation pitfall.
- **No open RFC §11 questions block this phase.** Q-4 (episodic memory) is post-V1.

## Glossary additions

- **`Summarizer`** — the injectable callable `rolling_summary` consumes. Single method `Summarize(ctx, id, req) (resp, error)`. Phase 24 ships only the interface + a stub `EchoSummarizer`; the LLM-backed implementation lands at Phase 32+. RFC §6.6.
- **`OverflowPolicy`** — buffer-overflow action under `truncation`. Phase 24 ships only `OverflowDropOldest` (departure from brief 04 §2's three-option enum; recorded in D-035). RFC §6.6.
- **`RecoveryBacklogMax`** — bounded queue size for the `rolling_summary` recovery loop. Default 16; overflow drops oldest and emits `memory.recovery_dropped`. RFC §6.6.
- **`memory.health_changed`** — bus event emitted on every `Health` transition under `rolling_summary`. `SafePayload` carries `(PriorHealth, NewHealth, Reason)`. The observable degradation path — the explicit exception to AGENTS.md §13's "no silent degradation" rule. RFC §6.6, D-035.
- **`memory.recovery_dropped`** — bus event emitted when the `rolling_summary` recovery backlog overflows `RecoveryBacklogMax`. `SafePayload` carries the drop reason; identity scopes the emit. RFC §6.6, D-035.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated targets (≥85% across `internal/memory`, `internal/memory/strategy`, `internal/memory/drivers/inmem`)
- [ ] Cross-session isolation test passes (inherited from Phase 23's conformance suite, now run against all three strategies)
- [ ] **Concurrent-reuse test passes** — `Concurrent_AllMethods_NoRace_Truncation` + `Concurrent_AllMethods_NoRace_RollingSummary` each running N≥128 goroutines under `-race`, no data races, no cross-identity bleed, no goroutine leaks (D-025).
- [ ] **Cross-subsystem integration test (`test/integration/memory_strategies_test.go`) wires real audit + events + state + memory drivers + the stub Summarizer, asserts identity propagation, covers ≥1 failure mode (forced summariser failure → degraded health transition observable on the bus), runs under `-race`.** Per AGENTS.md §17.
- [ ] New vocabulary added to glossary (yes — `Summarizer`, `OverflowPolicy`, `RecoveryBacklogMax`, `memory.health_changed`, `memory.recovery_dropped`).
- [ ] Brief-finding departures documented (OverflowPolicy narrowing + config-surface narrowing) + D-035 filed.
