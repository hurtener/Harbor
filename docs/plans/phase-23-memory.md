# Phase 23 — MemoryStore interface + InMem driver + conformance suite

## Summary

Land `internal/memory`: the single mandatory `MemoryStore` interface that every memory backend (in-memory now, SQLite + Postgres at Phase 25) implements; the `inmem` driver wired through the §4.4 driver-registry seam; and the cross-package `conformancetest.Run` suite that pins the contract for the persistent drivers landing in Phase 25. Strategy is `none` only at this phase — `truncation` + `rolling_summary` land in Phase 24. Identity is mandatory at the API boundary: missing tenant / user / session fails closed with `ErrIdentityRequired` AND emits a `memory.identity_rejected` audit event so the rejection is observable on the event bus. Memory records persist through Phase 07's `state.StateStore` via the typed-wrapper-over-generic pattern (D-027): the typed shape lives at this layer, the bytes are opaque to the StateStore.

## RFC anchor

- RFC §6.6
- RFC §9
- RFC §4
- RFC §3.5

## Briefs informing this phase

- brief 04

## Brief findings incorporated

- **brief 04 §1 (memory shape; identity is first-class via `context.Context`).** "Harbor inherits this shape cleanly and extends the isolation key from `(tenant, user, session)` config-resolved to a first-class `IdentityTriple` carried through `context.Context`." Phase 23 honours this: every `MemoryStore` method takes `ctx context.Context` and `id identity.Quadruple`; the `Quadruple` is validated at the boundary (mandatory triple; empty `RunID` allowed for session-scoped memory, mirroring Phase 07's StateStore rule).
- **brief 04 §2 (Go-flavored types: `MemoryStore`, `ConversationTurn`, `MemoryHealth`).** Phase 23 adopts the seven-method `MemoryStore` interface verbatim — `AddTurn / GetLLMContext / EstimateTokens / Flush / Health / Snapshot / Restore` — and the supporting `ConversationTurn`, `TrajectoryDigest`, `LLMContextPatch`, `Snapshot`, and `Health` types. The `Strategy` enum ships with `StrategyNone` only at this phase; `StrategyTruncation` + `StrategyRollingSummary` arrive at Phase 24 (constants declared as reserved so the type surface is stable but unsupported strategies are rejected by `Open` with a clear error).
- **brief 04 §4.1 ("`none` strategy semantics").** "`AddTurn` is a no-op; `GetLLMContext` returns empty." Phase 23's `inmem` driver implements exactly this — the strategy contract is preserved, even at the minimum-viable phase. `EstimateTokens` returns 0; `Flush` is a no-op; `Health` returns `HealthHealthy`; `Snapshot` returns an empty snapshot; `Restore` accepts only an empty snapshot (returns `ErrInvalidSnapshot` for non-empty restore against `Strategy=none` — fail loudly per AGENTS.md §5, not silent acceptance).
- **brief 04 §4.2 ("Identity-keyed isolation (fail-closed)").** "If the identity triple is incomplete, the operation behaves as if memory is disabled and emits an audit event, never returns data scoped to a default. The predecessor enforces this with `require_explicit_key=True` in `MemoryIsolation`; Harbor makes it the only mode by removing the toggle." Phase 23 implements this as a hard rule: missing identity returns `ErrIdentityRequired` (fail closed) AND emits `memory.identity_rejected` on the event bus (one event per rejection, identity field zeroed since the input was invalid; a typed `MemoryIdentityRejectedPayload` carries the operation name + reason). The rule applies to every method that accepts identity.
- **brief 04 §6 ("Isolation conformance (Harbor mandate)").** "Fail-closed: `MemoryStore` operation with a missing `SessionID` returns no data and emits an audit event." The conformance suite asserts this explicitly: a per-method test calls the store with a missing tenant / user / session and verifies (a) `errors.Is(err, ErrIdentityRequired)`, (b) the bus emitted exactly one `memory.identity_rejected` event for that call, (c) no data leaked across identities under concurrent invocation. The driver-injected bus seam keeps the test deterministic — no `time.Sleep` for synchronisation.
- **brief 04 §6 ("Cross-session no-leak (race)").** "100 concurrent sessions × random AddTurn / GetLLMContext / Snapshot for 30s under `-race`. Final invariant: every `GetLLMContext` output's identity matches the caller's identity exactly." Phase 23's `Concurrent_AddGet_NoRace` conformance test runs N≥128 goroutines × random `AddTurn / GetLLMContext / EstimateTokens / Snapshot / Restore / Flush / Health` ops against one shared driver instance under `-race`, asserts (a) no data races (race detector), (b) no goroutine leaks (`runtime.NumGoroutine` baseline restored), (c) per-method success, (d) no cross-identity bleed (each goroutine's reads return only its own bytes). Per D-025; same shape as the Phase 07 `state.conformancetest` concurrent test.

## Findings I'm departing from (if any)

- **brief 04 §4.2 names the audit event but does not name it.** I name it `memory.identity_rejected` (added to the events canonical registry via `events.RegisterEventType` at this phase's `init()`) — the naming is mine; the brief settled the requirement that an audit event MUST fire on missing-identity rejection. Recorded here so a later phase auditor doesn't flag the naming as drift.
- **The `Strategy` enum lands here with only `StrategyNone` operational.** Brief 04 §2 declares three strategy values; Phase 23's `Open` rejects `truncation` / `rolling_summary` with `ErrStrategyNotImplemented` so the strategy enum is forward-compatible (operators can stage the config field today; Phase 24 wires the implementations). Constants are exported so downstream code can reference them by name once Phase 24 lands.
- **No injectable `Summarizer` interface yet.** RFC §6.6 says "the summarizer is an injectable callable; the LLM call lives in the LLM-client subsystem; memory consumes a `Summarizer` interface" — that wiring is owned by Phase 24 (per its detail block in `docs/plans/README.md`) along with the strategies that consume it. Phase 23 keeps the surface minimal: `MemoryStore` does not name `Summarizer`. Recorded so Phase 24 doesn't re-litigate.

## Goals

- Single mandatory `MemoryStore` interface in `internal/memory` — seven methods (AddTurn / GetLLMContext / EstimateTokens / Flush / Health / Snapshot / Restore) plus `Close(ctx)`, no `Supports*` ceremony.
- `inmem` driver in `internal/memory/drivers/inmem/` that registers itself via `init()` and is blank-imported by `cmd/harbor/main.go`.
- Driver-registry seam (`Register` / `Open` / `OpenDriver` / `RegisteredDrivers`) modeled on `internal/state/registry.go` and `internal/events/registry.go`.
- Cross-package `conformancetest.Run(t, factory)` suite at `internal/memory/conformancetest/conformancetest.go` — same shape as Phase 01 + Phase 07 conformance suites. The InMem driver and every later driver (Phase 25) MUST pass this suite verbatim.
- Identity-mandatory at every method: `ErrIdentityRequired` returned AND `memory.identity_rejected` audit event emitted on the configured `events.EventBus`. The new event type registered in `init()` via `events.RegisterEventType`.
- D-027 typed-wrapper pattern: the `MemoryStore` interface owns the typed shape (`ConversationTurn`, `Snapshot`, `LLMContextPatch`, `Health`); the driver persists opaque bytes through `state.StateStore.Save(StateRecord{Kind: "memory.state", Bytes: marshal(record)})`. The `state.StateStore` is injected at `Open` time (per RFC §6.11 idiom — consuming subsystems land typed wrappers atop the generic surface).
- `MemoryConfig` added to `internal/config/config.go` with `Driver` + `Strategy` fields, validator coverage (`driver in {inmem}`, `strategy in {none}`, defaults applied). Documented in `examples/harbor.yaml`.
- Concurrent-reuse contract (D-025) enforced by the conformance suite: N≥128 goroutines exercising all methods against a single shared driver under `-race`, asserting no data races, no cross-identity bleed, no goroutine leaks.
- Cross-subsystem integration test in `test/integration/memory_state_test.go` per AGENTS.md §17: real audit + events + state + memory drivers wired end-to-end, identity propagation asserted, ≥1 failure mode (missing identity → audit event observable on the bus).

## Non-goals

- No `truncation` or `rolling_summary` strategies — Phase 24 owns those plus the `Summarizer` injectable.
- No SQLite or Postgres memory driver — Phase 25 ships those; they inherit this phase's conformance suite verbatim.
- No memory-budget enforcement (`Budget.TotalMaxTokens`, `OverflowPolicy`) — Phase 24 owns the budget logic (it ties to truncation).
- No `Summarizer` interface or LLM wiring — Phase 24 + Phase 32+ (LLM client).
- No memory health-state recovery loop — Phase 24 (lives with `rolling_summary`).
- No `IncludeTrajectory` / `TrajectoryDigest` ingestion path beyond the type definition — the planner runtime (Phase 42+) is the producer.
- No cross-session promotion (user-level / tenant-level memory) — that's a declared-policy post-V1 follow-up (RFC §6.6 "Memory budget at very long sessions — Tentative — see §11 Q-4").
- No HTTP / Protocol surface — memory is a Go-only surface at Phase 23. The smoke script SKIPs cleanly under preflight per AGENTS.md §4.1.

## Acceptance criteria

- [ ] `internal/memory/memory.go` defines `Strategy` (with `StrategyNone` operational and `StrategyTruncation` / `StrategyRollingSummary` declared as reserved), `Health`, `ConversationTurn`, `TrajectoryDigest`, `LLMContextPatch`, `Snapshot`, `MemoryStore` interface, sentinel errors (`ErrNotFound`, `ErrIdentityRequired`, `ErrUnknownDriver`, `ErrStoreClosed`, `ErrStrategyNotImplemented`, `ErrInvalidSnapshot`), and ctx helpers (`WithStore`, `MustFrom`, `From`).
- [ ] `internal/memory/registry.go` provides `Register(name, factory)`, `Open(ctx, cfg, deps) (MemoryStore, error)`, `OpenDriver(name, cfg, deps) (MemoryStore, error)`, `RegisteredDrivers() []string`. `Open` routes by `cfg.Driver`; unknown driver returns wrapped `ErrUnknownDriver` listing registered names. The `Deps` struct carries the injected `state.StateStore` (mandatory) and `events.EventBus` (mandatory for emitting the rejection audit event).
- [ ] `internal/memory/events.go` declares `EventTypeMemoryIdentityRejected = "memory.identity_rejected"`, registers it via `events.RegisterEventType` in `init()`, and exports the typed `MemoryIdentityRejectedPayload` (`SafePayload` by construction — fields are an `Operation` name + `Reason` string; no caller-controlled bytes).
- [ ] Every `MemoryStore` method (`AddTurn`, `GetLLMContext`, `EstimateTokens`, `Flush`, `Health`, `Snapshot`, `Restore`) validates the identity quadruple at the boundary. Empty tenant / user / session returns wrapped `ErrIdentityRequired` AND publishes one `memory.identity_rejected` event whose `Operation` field names the rejected method. Empty `RunID` is acceptable (session-scoped memory).
- [ ] The `memory.identity_rejected` event's `Identity` field carries whatever the caller supplied (zeroed or partial); subscribers MAY filter on `Admin: true` to fan-in cross-tenant rejections, or on a specific known triple. The bus's existing identity-mandatory filter rules already enforce normal subscriber-side scoping.
- [ ] `internal/memory/drivers/inmem/inmem.go` registers under name `inmem` via `init()`. Strategy `none`: `AddTurn` no-op; `GetLLMContext` returns empty `LLMContextPatch`; `EstimateTokens` returns 0; `Flush` no-op; `Health` returns `HealthHealthy`; `Snapshot` returns an empty `Snapshot{Strategy: StrategyNone}`; `Restore` accepts only empty snapshots and rejects non-empty with `ErrInvalidSnapshot`.
- [ ] The InMem driver persists every successful state change through `state.StateStore` as `StateRecord{Kind: "memory.state", Bytes: marshal(record)}` — even though strategy `none` has no state changes, the wiring is present so Phase 24's `truncation` / `rolling_summary` can reuse it. The marshalled record shape (`memoryStateRecord{Strategy, LastSnapshot}`) is internal to this package (not exported).
- [ ] `Open` rejects `Strategy = StrategyTruncation` or `StrategyRollingSummary` with `ErrStrategyNotImplemented`, naming the unsupported strategy. Phase 24 will swap in the real implementations.
- [ ] `cmd/harbor/main.go` blank-imports `_ "github.com/hurtener/Harbor/internal/memory/drivers/inmem"` (additive — does not disturb existing driver imports).
- [ ] `internal/config/config.go` `MemoryConfig` populated with `Driver string` + `Strategy string` + `BudgetTokens int` (reserved for Phase 24, validator only requires `>=0`). YAML tags use `omitempty` on `Strategy` + `BudgetTokens` so existing configs keep validating.
- [ ] `internal/config/validate.go` gains `validateMemory()` registered in `Validate()`'s slice. Rules: `Driver` must be in the allowlist `{inmem}` (Phase 25 will extend); `Strategy` must be empty (defaults to `none`) or `"none"` (Phase 24 will add `"truncation"` + `"rolling_summary"` to the allowlist). `BudgetTokens >= 0`.
- [ ] `examples/harbor.yaml` gains a `memory:` block with the new fields, commented and defaulting to `inmem` / `none`.
- [ ] `internal/memory/conformancetest/conformancetest.go` exports `Run(t *testing.T, factory func() (memory.MemoryStore, identity.Identity, events.EventBus, func()))`. Each subtest receives a fresh driver, the canonical identity to use, the bus the driver is wired to (so the test can subscribe and assert audit emits), and a cleanup closure. Subtests:
  - `AddTurn_NoOpForStrategyNone` — verifies `AddTurn` returns nil; `GetLLMContext` then returns empty.
  - `GetLLMContext_EmptyByDefault`.
  - `EstimateTokens_ReturnsZeroForStrategyNone`.
  - `Flush_NoOp`.
  - `Health_ReturnsHealthy`.
  - `Snapshot_Empty`.
  - `Restore_RoundTripsEmptySnapshot`.
  - `Restore_RejectsNonEmptyForStrategyNone` — `ErrInvalidSnapshot`.
  - `Identity_Mandatory_AddTurn` — empty tenant / user / session each → `ErrIdentityRequired` + bus emit of `memory.identity_rejected`.
  - `Identity_Mandatory_GetLLMContext` — same.
  - `Identity_Mandatory_AllMethods` — every method exercised with a malformed triple; asserts one `memory.identity_rejected` event per method per call.
  - `CrossTenant_Isolation` — two identities never see each other's records (the InMem driver-Strategy=none has no records; the test asserts the StateStore wiring underneath is keyed per-identity by writing a Snapshot/Restore round-trip and asserting cross-tenant Load returns `ErrNotFound`).
  - `CrossSession_Isolation` — same shape, same tenant + user, different sessions.
  - `Concurrent_AllMethods_NoRace` — N≥128 goroutines × random op exercising every method; asserts no races, no goroutine leak, no cross-identity bleed. D-025.
  - `Close_Idempotent` — `Close` called twice returns nil.
  - `AfterClose_OperationsError` — all methods return `ErrStoreClosed` after `Close`.
- [ ] `internal/memory/conformancetest/conformancetest_test.go` self-applies `Run` against the InMem driver factory.
- [ ] `internal/memory/memory_test.go` covers the registry surface (`Register` panic on duplicate / nil / empty; `Open` routes; unknown driver wraps `ErrUnknownDriver` with registered list) and the sentinel-error wiring at the package boundary.
- [ ] `internal/memory/drivers/inmem/inmem_test.go` invokes the conformance suite against the local driver.
- [ ] `test/integration/memory_state_test.go` (new) wires real audit + events + state + memory drivers end-to-end. Asserts: (a) round-trip Open → AddTurn (no-op) → Snapshot → Restore; (b) identity propagation through every layer; (c) failure mode — a call with missing identity returns `ErrIdentityRequired` AND the bus delivers exactly one `memory.identity_rejected` event with the expected Operation field.
- [ ] Test coverage on `internal/memory`: ≥ 85% (per master plan's coverage target for Phase 23).
- [ ] `make drift-audit` and `make preflight` green at commit time.
- [ ] `scripts/smoke/phase-23.sh` present and executable; SKIPs cleanly (no HTTP surface).
- [ ] `docs/glossary.md` gains entries for `MemoryStore`, `ConversationTurn`, `MemoryHealth`, `MemorySnapshot`, `LLMContextPatch`, `memory.identity_rejected` event.
- [ ] `docs/decisions.md` gains entry **D-033** documenting the memory record persistence key (`Kind = "memory.state"`) and the fail-closed-with-audit-emit pattern for identity rejection. (Numbered by reading the last decision in the file and adding 1.)
- [ ] `docs/plans/README.md` flips Phase 23's status from `Pending` to `Shipped`.
- [ ] `README.md` Status table gains a Phase 23 row.

## Files added or changed

- `internal/memory/memory.go` (new) — interface + types + sentinel errors + ctx helpers.
- `internal/memory/registry.go` (new) — `Register / Open / OpenDriver / RegisteredDrivers` + `Deps` struct.
- `internal/memory/events.go` (new) — `EventTypeMemoryIdentityRejected` constant + `RegisterEventType` init + `MemoryIdentityRejectedPayload` (SafePayload).
- `internal/memory/memory_test.go` (new) — registry-surface unit tests.
- `internal/memory/conformancetest/conformancetest.go` (new) — exported `Run(t, factory)`; subpackage chosen so `internal/memory` does not import `testing` (precedent: Phase 01 + Phase 07).
- `internal/memory/conformancetest/conformancetest_test.go` (new) — self-applied smoke against InMem driver.
- `internal/memory/drivers/inmem/inmem.go` (new) — InMem driver; `init()` registers under `"inmem"`.
- `internal/memory/drivers/inmem/inmem_test.go` (new) — driver-level tests + invokes the conformance suite.
- `internal/config/config.go` (modified) — populate the previously-reserved `MemoryConfig` struct.
- `internal/config/validate.go` (modified) — add `validateMemory()` to the Validate slice; new allowlists for memory driver and strategy.
- `internal/config/validate_test.go` (modified) — add validator coverage for the new fields.
- `internal/config/loader.go` (modified, if needed) — apply defaults (`Driver=inmem`, `Strategy=none`) when the section is omitted.
- `internal/config/loader_test.go` (modified, if needed) — defaults coverage.
- `cmd/harbor/main.go` (modified) — additive blank import for the InMem memory driver.
- `examples/harbor.yaml` (modified) — add a commented `memory:` block.
- `test/integration/memory_state_test.go` (new) — cross-subsystem integration test per AGENTS.md §17.
- `scripts/smoke/phase-23.sh` (new) — SKIP-only smoke (no HTTP surface; precedent: phase-07.sh).
- `docs/plans/phase-23-memory.md` (this file).
- `docs/plans/README.md` (modified) — flip Phase 23 status `Pending → Shipped`.
- `docs/glossary.md` (modified) — add entries listed under Acceptance.
- `docs/decisions.md` (modified) — add D-033.
- `README.md` (modified) — add Phase 23 to the Status table.

No top-level directory additions — `internal/memory/` is already enumerated in AGENTS.md §3.

## Public API surface

```go
package memory

import (
    "context"
    "errors"
    "time"

    "github.com/hurtener/Harbor/internal/events"
    "github.com/hurtener/Harbor/internal/identity"
    "github.com/hurtener/Harbor/internal/state"
)

// Strategy declares the memory shape the store applies. Phase 23 ships
// StrategyNone operational; Phase 24 adds StrategyTruncation +
// StrategyRollingSummary.
type Strategy string

const (
    StrategyNone           Strategy = "none"
    StrategyTruncation     Strategy = "truncation"      // Phase 24
    StrategyRollingSummary Strategy = "rolling_summary" // Phase 24
)

// Health enumerates the memory subsystem health states. Phase 23 only
// produces HealthHealthy; Phase 24 produces the full FSM
// (healthy → retry → degraded → recovering → healthy) for
// rolling_summary.
type Health string

const (
    HealthHealthy    Health = "healthy"
    HealthRetry      Health = "retry"      // Phase 24
    HealthDegraded   Health = "degraded"   // Phase 24
    HealthRecovering Health = "recovering" // Phase 24
)

// ConversationTurn is one turn of a memory-tracked conversation.
// Producers (planner runtime, Phase 42+) hand turns to AddTurn.
type ConversationTurn struct {
    UserMessage         string
    AssistantResponse   string
    TrajectoryDigest    *TrajectoryDigest
    ArtifactsShown      map[string]any
    ArtifactsHiddenRefs []string
    Timestamp           time.Time
}

// TrajectoryDigest is the compact planner-side trace snapshot the
// memory subsystem MAY persist alongside the turn. Phase 23 does not
// ingest it (Strategy=none); the type ships now so Phase 24 +
// downstream planner phases share one definition.
type TrajectoryDigest struct {
    ToolsInvoked        []string
    ObservationsSummary string
    ReasoningSummary    string
    ArtifactsRefs       []string
}

// LLMContextPatch is the output GetLLMContext returns: the patch a
// planner runtime applies to its LLM call. Strategy=none returns an
// empty patch; later strategies return rolling text, summary blocks,
// and turn fragments.
type LLMContextPatch struct {
    Strategy  Strategy
    Summary   string
    RecentTurns []ConversationTurn
    Tokens    int
}

// Snapshot is the export shape for Snapshot / Restore. Round-trips
// the strategy plus opaque driver bytes; consumers (Protocol surface
// at Phase 60+, Console) see the typed shape, drivers see only Bytes.
type Snapshot struct {
    Strategy Strategy
    Bytes    []byte
}

// MemoryStore is Harbor's mandatory memory surface — single
// interface, every V1 driver (inmem here, sqlite + postgres at
// Phase 25) implements every method. No `Supports*` ceremony.
//
// Identity-mandatory: every method validates the triple
// (tenant/user/session) at the boundary; empty triple returns
// wrapped ErrIdentityRequired AND emits memory.identity_rejected on
// the configured EventBus (D-001 + the brief-04-§4.2 fail-closed
// rule).
//
// Concurrent-reuse safe (D-025): one instance is safe for N
// goroutines against a single shared instance.
type MemoryStore interface {
    AddTurn(ctx context.Context, id identity.Quadruple, turn ConversationTurn) error
    GetLLMContext(ctx context.Context, id identity.Quadruple) (LLMContextPatch, error)
    EstimateTokens(ctx context.Context, id identity.Quadruple) (int, error)
    Flush(ctx context.Context, id identity.Quadruple) error
    Health(ctx context.Context, id identity.Quadruple) (Health, error)
    Snapshot(ctx context.Context, id identity.Quadruple) (Snapshot, error)
    Restore(ctx context.Context, id identity.Quadruple, snap Snapshot) error
    Close(ctx context.Context) error
}

// Deps carries the runtime dependencies the memory subsystem needs.
// The StateStore is required (D-027 — typed wrapper writes opaque
// bytes through the generic surface); the EventBus is required so
// identity-rejection emits land on the bus.
type Deps struct {
    State state.StateStore
    Bus   events.EventBus
}

// Factory builds a MemoryStore from a MemoryConfig + Deps. Drivers
// expose one Factory each via init() → Register.
type Factory func(cfg ConfigSnapshot, deps Deps) (MemoryStore, error)

// ConfigSnapshot is the strict subset of config.MemoryConfig the
// memory package consumes. Keeping a snapshot decouples drivers from
// the config package.
type ConfigSnapshot struct {
    Driver       string
    Strategy     Strategy
    BudgetTokens int
}

// Register / Open / OpenDriver / RegisteredDrivers — the §4.4 seam.

// Sentinel errors. Compared via errors.Is.
var (
    ErrNotFound              = errors.New("memory: record not found")
    ErrIdentityRequired      = errors.New("memory: identity triple incomplete")
    ErrUnknownDriver         = errors.New("memory: unknown driver")
    ErrStoreClosed           = errors.New("memory: store is closed")
    ErrStrategyNotImplemented = errors.New("memory: strategy not implemented at this phase")
    ErrInvalidSnapshot       = errors.New("memory: invalid snapshot for this strategy")
)
```

`MemoryStore`, the supporting types, the sentinel errors, the registry functions, the `Deps` and `ConfigSnapshot` structs, and the `conformancetest.Run` suite are the entire public surface.

## Test plan

- **Unit:**
  - Registry surface — `Register` panics on duplicate / empty / nil; `Open` routes by `cfg.Driver`; unknown driver wraps `ErrUnknownDriver` with the registered list.
  - Sentinel-error formatting / wrapping.
  - `validateMemory` — driver allowlist, strategy allowlist, `BudgetTokens >= 0`.
  - Defaults — config loader fills `Driver=inmem`, `Strategy=none` when the section is omitted.
- **Integration:**
  - `test/integration/memory_state_test.go` wires audit + events + state + memory end-to-end. Asserts identity propagation, round-trip success path, fail-closed-on-missing-identity with audit event observable on the bus. Per AGENTS.md §17 (Deps = `01, 07` → cross-subsystem integration test required).
- **Conformance:**
  - `conformancetest.Run` is the load-bearing test surface (subtests enumerated under Acceptance). Self-applied against InMem; Phase 25 SQLite + Postgres drivers will consume the same suite verbatim.
- **Concurrency / leak (D-025):**
  - `Concurrent_AllMethods_NoRace` — N≥128 goroutines × every-method-op against one shared driver under `-race`; asserts no data races, no goroutine leaks (`runtime.NumGoroutine` baseline restored), no cross-identity bleed.
  - `GoroutineLeak_AfterClose` — driver Close + Cleanup returns to baseline goroutine count.

## Smoke script additions

- `scripts/smoke/phase-23.sh` issues `skip "phase 23: memory store — Go package only; validated by go test ./internal/memory/..."` and calls `smoke_summary`. Phase 23 has no HTTP / Protocol surface; correctness is verified by `go test -race ./internal/memory/...` and the integration test. SKIP increments cleanly under preflight; no FAIL.

## Coverage target

- `internal/memory`: 85% (registry + sentinel surface + ctx helpers).
- `internal/memory/drivers/inmem`: 90% (driver implements every interface method; the conformance suite drives every code path).
- `internal/memory/conformancetest`: not gated; the helper's `t.Errorf` / `t.Fatalf` paths only fire when a downstream driver fails the suite (precedent: Phase 01 + Phase 07 conformance subpackages).

## Dependencies

- Phase 01 (identity) — `identity.Quadruple` is the storage key.
- Phase 07 (state) — `state.StateStore` is the persistence floor; the memory driver writes typed records through it per D-027.
- Phase 03 (audit) — declared logically: the runtime is wired with a Redactor in the audit context; this phase does not run redaction directly (the events package handles the audit-redactor pass during Publish for non-Safe payloads). `MemoryIdentityRejectedPayload` is a `SafePayload` so it skips the redactor — fields are bounded enumerable strings (the operation name + a static reason), no caller-controlled bytes.
- Phase 05 (events) — `events.EventBus` is required to emit `memory.identity_rejected` on the fail-closed identity-rejection path. The new event type is registered via `events.RegisterEventType` in this phase's `init()`.

## Risks / open questions

- **Audit-event emit on a missing-identity path: where does the Event.Identity come from?** The bus already enforces an identity-mandatory `ValidateEvent` (`internal/events/events.go::ValidateEvent`) — events with empty tenant/user/session are rejected with `ErrIdentityRequired`. For the rejection event to be publishable, it must carry SOME identity. Two options: (a) emit on a separate "system" identity (e.g. `{TenantID: "system", UserID: "memory", SessionID: "identity-rejected"}`); (b) emit synchronously with whatever partial identity the caller supplied, defaulting the missing component to a sentinel like `"<missing>"`. I take option (b) — partial identity is preserved (matches D-001's "fail closed with the rejected scope visible") and the substituted-defaults sentinel `"<missing>"` signals the rejection cleanly in event payloads. This decision is recorded in D-033.
- **Strategy-rejected error from `Open` vs from the driver?** Phase 23 ships strategy=none operational; `Open` for any other strategy returns `ErrStrategyNotImplemented` from the factory (`inmem.New`). Phase 24 will swap the factory's strategy switch when truncation + rolling_summary land. This way today's operator config can already carry `strategy: none` (the default) and a future config change activates Phase 24's strategies without code changes outside the driver.
- **`memoryStateRecord` shape inside the driver — what shape do we persist for Strategy=none?** The driver still writes a `state.StateStore.Save` for the "memory state" record on every method that mutates state (Phase 23 has no mutations under Strategy=none, so the `state.StateStore` is touched only on `Restore` for empty-snapshot acceptance). The persisted bytes are `{"strategy":"none","turns":[]}` — a JSON-serialised internal `memoryStateRecord{Strategy Strategy, Turns []ConversationTurn}`. Phase 24 will append `Turns`; Phase 25 swaps the driver but the persisted shape is unchanged.
- **No open RFC §11 questions block this phase.** The post-V1 episodic-memory question (Q-4) is explicitly Phase 24+ scope.

## Glossary additions

- **`MemoryStore`** — Harbor's mandatory memory interface. Seven methods (`AddTurn / GetLLMContext / EstimateTokens / Flush / Health / Snapshot / Restore`) plus `Close`. Three V1 drivers will ship — `inmem` (Phase 23), `sqlite` + `postgres` (Phase 25) — all passing the same `conformancetest.Run` suite. Identity-mandatory at every method; fail-closed on missing triple with `memory.identity_rejected` audit emit. RFC §6.6, AGENTS.md §6, D-001, D-027.
- **`ConversationTurn`** — one turn of a memory-tracked conversation (`UserMessage`, `AssistantResponse`, optional `TrajectoryDigest`, artifact references). The planner runtime (Phase 42+) is the producer; `MemoryStore.AddTurn` is the consumer. RFC §6.6.
- **`MemoryHealth`** — `MemoryStore.Health` return: `healthy | retry | degraded | recovering`. Phase 23 only produces `healthy` (Strategy=none); Phase 24's `rolling_summary` drives the full FSM. RFC §6.6.
- **`MemorySnapshot`** — the export shape returned by `MemoryStore.Snapshot` and consumed by `Restore`. Carries `(Strategy, Bytes)`. Bytes are opaque to consumers; only the same driver shape can `Restore` them. RFC §6.6.
- **`LLMContextPatch`** — the patch a planner runtime applies to its LLM call after `MemoryStore.GetLLMContext`. Carries `(Strategy, Summary, RecentTurns, Tokens)`. Empty under Strategy=none. RFC §6.6.
- **`memory.identity_rejected`** — bus event emitted when a `MemoryStore` method is called with an incomplete identity triple. `SafePayload` (bounded operation name + static reason; no caller-controlled bytes). Subscribers MAY admin-scope-filter for cross-tenant fan-in. RFC §6.6, D-001.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated targets (85% `internal/memory`, 90% `internal/memory/drivers/inmem`)
- [ ] Cross-session isolation test passes (`CrossTenant_Isolation` + `CrossSession_Isolation` in the conformance suite)
- [ ] **Concurrent-reuse test passes** — `Concurrent_AllMethods_NoRace` in the conformance suite, N≥128 goroutines under `-race`, no data races, no cross-identity bleed, no goroutine leaks (D-025).
- [ ] **Cross-subsystem integration test (`test/integration/memory_state_test.go`) wires real audit + events + state + memory drivers, asserts identity propagation, covers ≥1 failure mode (missing identity → audit event observable on the bus), runs under `-race`.** Per AGENTS.md §17.
- [ ] New vocabulary added to glossary (yes — `MemoryStore`, `ConversationTurn`, `MemoryHealth`, `MemorySnapshot`, `LLMContextPatch`, `memory.identity_rejected`).
- [ ] Brief-finding departures documented (Strategy enum partial-implementation rationale + audit-event naming rationale) + D-033 filed.
