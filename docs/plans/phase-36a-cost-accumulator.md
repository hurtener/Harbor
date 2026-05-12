# Phase 36a — Cost accumulator + per-identity ceilings (governance)

## Summary

Establish the `governance.Subsystem` interface with `PreCall` / `PostCall`
hooks that wrap the LLM-edge chain (composing OUTSIDE Phase 36's retry
wrapper). Ship a StateStore-backed `CostAccumulator` that aggregates LLM
costs per `(tenant, user, session, model)`; ceilings are operator-configured
per identity tier. Latent default: empty config → no enforcement.

## RFC anchor

- RFC §6.15

## Briefs informing this phase

- brief 03
- brief 06

## Brief findings incorporated

- **brief 03 §6:** Bifrost surfaces cost via `Usage.Cost.TotalCost`. Phase 33
  already wires the bifrost driver to publish `llm.cost.recorded` on every
  successful Complete. Phase 36a consumes the same cost figures via the
  `PostCall` hook on the same response, not the event — see departure.
- **brief 06 §3:** Identity-scoped subscriptions filter by the canonical
  triple; admin-scope subscribers fan in cross-tenant. Governance event
  types (`governance.budget_exceeded` / `governance.rate_limited` /
  `governance.maxtokens_exceeded`) compose with that subscription model
  unchanged. Phase 36a registers the budget event type; 36b registers the
  other two.

## Findings I'm departing from (if any)

- **The accumulator update path is `PostCall` (synchronous), NOT a
  subscription to `llm.cost.recorded`.** The parent dispatch suggested a
  bus-subscriber accumulator path; the RFC §6.15 wording at line 1128
  ("PostCall... Accumulates cost / tokens / latency") is unambiguous and
  binding. The race window in an async-subscriber path would let PreCall
  on call N+1 check the ceiling before call N's cost was applied —
  unacceptable for ceiling enforcement correctness. The `llm.cost.recorded`
  event remains the operator-facing observability signal (Console
  subscribes when it lands); governance's accumulator is its own internal
  state. Settled in D-044.

## Goals

- Define `governance.Subsystem` interface (`PreCall` / `PostCall`).
- Ship a `Wrap(inner llm.LLMClient, sub Subsystem) llm.LLMClient` that
  composes OUTSIDE Phase 36's retry wrapper: governance is the outermost
  layer in the chain.
- Ship `CostAccumulator` Subsystem with operator-configured ceilings per
  identity tier.
- Persist accumulator state to `state.StateStore` so it survives runtime
  restart. Three-driver conformance (in-mem / SQLite / Postgres).
- Lock-free atomic accumulator math under high concurrency.
- StateStore read failure → fail loudly (no silent permit).
- Register `governance.budget_exceeded` event type.
- Latent default: empty `Governance.IdentityTiers` map → no ceiling →
  PreCall permits unconditionally.

## Non-goals

- Token bucket rate limits (Phase 36b).
- Per-call MaxTokens enforcement (Phase 36b).
- Refunds on call failure (RFC §6.15 simplicity — drain-on-PreCall is final
  for rate limits; cost is added on PostCall regardless of `callErr`).
- Hot-reload of ceilings (restart-required at V1; D-NNN tracking).
- Tier resolution from claims / JWT — Phase 36a accepts a `TierResolver`
  function pointer; the default returns `Config.DefaultTier` for every
  identity. Custom resolvers are operator-supplied at construction.
- Protocol-driven setters for ceilings (post-V1 phase 91 per master plan).

## Acceptance criteria

- [ ] `governance.Subsystem` interface ships with `PreCall(ctx, req)
      error` + `PostCall(ctx, req, resp, callErr) error`.
- [ ] `governance.Wrap(inner, sub)` returns an `llm.LLMClient` whose
      `Complete` invokes `sub.PreCall` then the inner client then
      `sub.PostCall`. PreCall returning non-nil short-circuits without
      calling the inner.
- [ ] Identity-mandatory: PreCall reads identity from ctx; missing →
      fail-closed with wrapped error.
- [ ] `CostAccumulator.PostCall` aggregates `resp.Cost.TotalCost` per
      `(tenant, user, session, model)` AND per `(tenant, user, session)`.
- [ ] State persists across `Close` + reopen with the same StateStore.
- [ ] Three-driver conformance (in-mem / SQLite / Postgres) green for the
      cost accumulator.
- [ ] Ceiling check: `PreCall` returns wrapped `ErrBudgetExceeded` and
      emits `governance.budget_exceeded` when the (identity, tier) total
      cost ≥ tier ceiling.
- [ ] **Empty config → no enforcement.** With `Governance.IdentityTiers`
      empty / `DefaultTier` empty, every PreCall returns nil and no event
      emits.
- [ ] StateStore read failure on PreCall lookup → wrapped error (not a
      silent permit).
- [ ] Cross-session isolation: tenant A's accumulator does not affect
      tenant B's ceiling check.
- [ ] **Concurrency gate**: N≥100 concurrent calls against ONE shared
      `CostAccumulator` with a low ceiling. After every call returns,
      total cost ≤ ceiling + (per-call-max-cost × in-flight). Lock-free
      atomic CAS pinned.
- [ ] D-025 concurrent-reuse: N≥100 concurrent invocations against ONE
      shared Subsystem instance.
- [ ] `scripts/smoke/phase-36a.sh` green.
- [ ] Coverage on `internal/governance`: ≥ 85%.

## Files added or changed

- `internal/governance/governance.go` (new) — `Subsystem` interface +
  shared types + clock.
- `internal/governance/wrap.go` (new) — `Wrap(inner, sub) llm.LLMClient`.
- `internal/governance/errors.go` (new) — sentinel errors.
- `internal/governance/events.go` (new) — `governance.budget_exceeded`
  event type + `BudgetExceededPayload`.
- `internal/governance/cost.go` (new) — `CostAccumulator` Subsystem.
- `internal/governance/compound.go` (new) — `CompoundSubsystem` that
  fans PreCall/PostCall across multiple sub-Subsystems.
- `internal/governance/registry.go` (new) — `llm.RegisterGovernanceWrapper`
  hook installation + factory; blank-imported in `cmd/harbor/main.go`.
- `internal/governance/cost_test.go` + `governance_test.go` +
  `wrap_test.go` + `compound_test.go` (new).
- `internal/governance/conformancetest/` (new) — shared StateStore
  conformance suite for the cost accumulator.
- `internal/governance/conformance_inmem_test.go` (new) — drives the
  conformance suite against the in-mem state driver.
- `internal/llm/registry.go` (modified) — adds `RegisterGovernanceWrapper`
  hook + composes the governance wrapper outermost in `Open`.
- `internal/config/config.go` (modified) — extend `GovernanceConfig` with
  `IdentityTiers` + `DefaultTier` + `TierConfig`.
- `internal/config/validate.go` (modified) — `validateGovernance` extends.
- `internal/config/loader.go` (modified) — defaults.
- `cmd/harbor/main.go` (modified) — blank-import.
- `examples/harbor.yaml` (modified) — sample latent + opt-in tier blocks.
- `scripts/smoke/phase-36a.sh` (new) — smoke.
- `README.md` (modified) — Phase 36a row → Shipped.
- `docs/plans/README.md` (modified) — Phase 36a row → Shipped.
- `docs/decisions.md` (modified) — D-044.
- `docs/glossary.md` (modified) — new vocabulary.

## Public API surface

```go
package governance

type Subsystem interface {
    PreCall(ctx context.Context, req llm.CompleteRequest) error
    PostCall(ctx context.Context, req llm.CompleteRequest, resp llm.CompleteResponse, callErr error) error
}

func Wrap(inner llm.LLMClient, sub Subsystem) llm.LLMClient

type Config struct {
    DefaultTier   string
    IdentityTiers map[string]TierConfig
    Resolver      TierResolver
    Clock         Clock
}

type TierConfig struct {
    BudgetCeilingUSD float64
    RateLimit        RateLimitConfig
    MaxTokens        int
}

type RateLimitConfig struct {
    Capacity        int
    RefillTokens    int
    RefillInterval  time.Duration
}

type TierResolver func(identity.Identity) string

func NewCostAccumulator(state state.StateStore, bus events.EventBus, cfg Config) (*CostAccumulator, error)
func NewRateLimiter(state state.StateStore, bus events.EventBus, cfg Config) (*RateLimiter, error)        // Phase 36b
func NewMaxTokensEnforcer(bus events.EventBus, cfg Config) *MaxTokensEnforcer                            // Phase 36b
func NewCompound(subs ...Subsystem) Subsystem
```

`llm.RegisterGovernanceWrapper(fn)` is the registration hook; the
governance package's `init()` registers the wrapper hook when blank-
imported.

## Test plan

- **Unit:** Subsystem interface contract + Wrap composition + Cost
  accumulator math + tier lookup + ceiling check + identity isolation +
  error classification.
- **Integration:** `Wrap(safetyClient(driver))` composes end-to-end with
  the mock LLM driver. PreCall blocks on exceedance; PostCall accumulates
  on success.
- **Conformance:** Three-driver suite (in-mem / SQLite / Postgres) for
  accumulator persistence + restart-survival. (Per-driver test files in
  `internal/state/drivers/<driver>` glue to the conformance suite.)
- **Concurrency / leak:** N≥100 concurrent calls under `-race` against one
  CostAccumulator. Total cost ≤ ceiling + permitted overshoot. No
  goroutine leaks (baseline restored after teardown). D-025 reuse test.

## Smoke script additions

- `scripts/smoke/phase-36a.sh` exercises the package under `-race` and
  asserts: governance event types are registered; the wrapper composes
  via the hook; the static no-tool-call-API guard extends to
  `internal/governance/`.

## Coverage target

- `internal/governance`: ≥ 85%.

## Dependencies

- 11 (event bus skeleton — `governance.budget_exceeded` lives there).
- 15 (StateStore SQLite driver — accumulator persistence).
- 33 (bifrost integration — `Usage.Cost.TotalCost` flows here).
- 36 (retry-with-feedback wrapper — governance composes OUTSIDE it).

## Risks / open questions

- **Atomic float64 accumulator races.** Mitigated by a CAS loop over
  `math.Float64bits` of the per-(identity, model) sum. The concurrency
  test under `-race` is the gate.
- **PreCall vs PostCall race window.** Even with atomic increments,
  N concurrent in-flight PreCalls can each see "below ceiling" simultaneously
  → all calls proceed → all PostCalls add → total may exceed ceiling by
  up to N × per-call-cost. This is RFC §6.15-acceptable: the master plan
  test wording is "do not overshoot ceiling" not "overshoot by zero." We
  document the tolerance as `permitted_overshoot = in_flight_max ×
  per_call_max_cost` and assert in tests. Operators who need stricter
  semantics get them post-V1 via the unified pause/resume primitive
  (RFC §6.15 line 1181 — pause on first-cross).
- **Bus-publish failure during emit.** Best-effort; emit error logged but
  not returned to caller — the ceiling check ALREADY fired, the event is
  a notification. Documented in code.

## Glossary additions

- `Subsystem` (governance)
- `PreCall` / `PostCall`
- `CostAccumulator`
- `IdentityTier`
- `BudgetCeiling`
- `TierResolver`

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Cross-session isolation test passes
- [ ] Concurrent-reuse test passes — N≥100 invocations under `-race`
- [ ] Integration test wires real drivers (state inmem + events inmem +
      mock LLM); identity propagation; failure-mode coverage
- [ ] Glossary updated
- [ ] D-044 added to `docs/decisions.md`
