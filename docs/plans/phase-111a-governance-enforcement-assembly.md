# Phase 111a — Governance enforcement assembly

## Summary

Governance enforcement is a fully-built primitive with zero production consumers.
The enforcers all exist and pass conformance — `NewCostAccumulator`
(`internal/governance/cost.go:105`), `NewRateLimiter`
(`internal/governance/ratelimit.go:80`), `NewMaxTokensEnforcer`
(`internal/governance/maxtokens.go:30`), `NewCompound`
(`internal/governance/compound.go:28`) — and the `llm.Open` composition seam is
live (the wrapper hook installed by `internal/governance/registry.go`'s `init()`).
But `governance.SetFactory`'s ONLY caller is a test
(`internal/governance/registry_test.go:97`). An operator who populates
`governance.identity_tiers` in `harbor.yaml` today gets **posture display and
zero enforcement**: the config drives only the read-only posture provider
(`cmd/harbor/cmd_dev.go:1145`, `harbortest/devstack/devstack.go:984`). Clean
validation, silent no-op — the exact fail-loud violation the SDK friction audit
(`docs/notes/sdk-friction-audit.md` §1 + §3) flagged, and a §13
primitive-without-consumer standing violation.

Phase 111a ships the assembly: an exported
`governance.NewSubsystemFromConfig(cfg, store, bus)` that composes the three
enforcers in the documented order, called via `SetFactory` from the production
assembly whenever `IdentityTiers` is non-empty. A configured tier then actually
rejects and limits — cost ceiling, rate limit, and MaxTokens each exercised
end-to-end. The posture-only boot warning the in-flight Wave A chore adds
(PR #278, per audit §8 Wave A: "until then
`validateGovernance` must warn posture-only") is removed in the same PR — the
warning's reason to exist is gone.

## RFC anchor

- RFC §6.15 — Governance subsystem (the V1 scope this phase finally wires:
  cost accumulator + ceilings, rate limits, per-call MaxTokens; the
  "Governance wraps the `LLMClient` interface" boundary; D-043/D-044 compose
  order).
- RFC §6.5 — LLM client layer (the `llm.Open` wrapper chain governance
  composes outside of).
- RFC §6.11 — StateStore (the persistence floor the accumulators ride on).

## Briefs informing this phase

- brief 03 — tools-and-llm (the LLM-client cost/provenance surface governance
  consumes)
- brief 08 — LLM client validation (bifrost's cost reporting — the empirical
  basis for the cost accumulator's input)

## Brief findings incorporated

- **brief 03 §provenance uniformity.** "The planner, the event bus, and the
  artifact store care about `(tool_name, args, result, source_id, transport,
  latency, cost)`" — cost is first-class provenance. The enforcement subsystem
  consumes the same canonical cost stream the posture provider displays; this
  phase does not mint a second cost path, it wires the existing one to the
  existing enforcers.
- **brief 08 §usage/cost validation.** "All six models … reported usage tokens,
  and reported cost in USD" through bifrost's pass-through
  (`Usage.Cost.TotalCost`). The cost accumulator's input is real on the
  production driver today — there is no missing-data excuse for leaving
  enforcement latent.
- **brief 03 §two-parallel-modes smell.** "Two modes shipping in parallel …
  is an anti-pattern." Posture-display-without-enforcement is that smell one
  layer up: the config knob ships two behaviours (display vs. enforce) and
  silently delivers only one. This phase collapses them: populated tiers mean
  enforcement, full stop.

## Findings I'm departing from (if any)

None.

## Goals

- **Exported assembly entry:**
  `governance.NewSubsystemFromConfig(cfg governance.Config, store state.StateStore, bus events.EventBus) (governance.Subsystem, error)`
  in `internal/governance/governance.go` (or a sibling `assembly.go`):
  - Composes `NewCompound(NewMaxTokensEnforcer(bus, cfg), rateLimiter,
    costAccumulator)` in the documented cheapest-reject-first order
    (`compound.go`'s godoc: MaxTokens → RateLimiter → CostAccumulator).
  - Returns `(nil, nil)` when `cfg.IdentityTiers` is empty — preserving the
    D-044 latent default exactly (the wrapper hook treats a nil Subsystem as
    pass-through). An empty-tiers map is the ONE sanctioned "no enforcement"
    state, and it is visible in posture, not silent.
  - Fails loudly (`ErrInvalidConfig`) on nil store / nil bus when tiers are
    non-empty — enforcement without persistence or observability is not a
    degraded mode, it's a misconfiguration.
- **Production consumer in the same phase (§13):** the production assembly
  calls `governance.SetFactory` when `cfg.Governance.IdentityTiers` is
  non-empty, with a closure capturing the assembly's `governance.Config` +
  `state.StateStore`; the factory builds via `NewSubsystemFromConfig` using the
  `(llm.ConfigSnapshot, llm.Deps)` pair `llm.Open` hands it (the bus comes from
  `Deps.Bus`). `SetFactory` is invoked BEFORE `llm.Open` in the boot order.
  - **Assembly target:** Wave B's 110d (the promoted, error-returning
    `assemble.Assemble`) has merged (D-197) — the wiring lands there: one
    site, cmd + devstack are thin callers. No D-094 hand-mirror is needed.
- **Config projection:** consumes 110c's exported
  `governance.ConfigFromOperator` (the config→`governance.Config` projection
  that replaces the unexported `governanceConfigFromConfig` /
  `governanceConfigForDevstack` duplicate pair). If 110c has not merged, this
  phase promotes that projection itself with the same name — whichever lands
  second deletes the duplicate (soft dependency, not a blocker).
- **Per-Open option vs. process-global `SetFactory` — evaluated and decided.**
  The factory seam is process-global (`registry.go:43`); a multi-runtime
  embedder running two stacks with different tier maps in one process would
  collide (second `SetFactory` wins). Decision for this phase: **keep
  `SetFactory`** as the binary's zero-ceremony path — it is the settled D-044
  shape, `llm.Open` already consults it, and the binary assembles exactly one
  stack. The multi-runtime escape **already exists and gets documented as the
  SDK path**: `governance.Wrap(client, sub)` is exported — an embedder
  constructs `NewSubsystemFromConfig` per stack and wraps each `llm.Open`
  result directly (governance stays outermost per D-043). No new `llm.Open`
  option is minted for a consumer that doesn't exist yet; the decision +
  the documented escape are recorded in D-198 (reserved; logged when the
  phase ships).
- **Remove the posture-only warning:** the Wave A chore's
  `validateGovernance` "identity_tiers configured but enforcement is
  posture-only" warning is deleted in this PR (its condition can no longer
  occur). §17.6: grep both validate and any boot-banner site.
- **E2E enforcement proof:** a configured tier actually gates calls — all
  three enforcers exercised (see Test plan).

## Non-goals

- No post-V1 governance capabilities (key rotation, model swap, failover,
  circuit breakers, LLM cache — master plan phases 91–96).
- No new policy shapes or tier-config fields — `governance.Config` /
  `TierConfig` are consumed as shipped by 36a/36b.
- No pause/resume routing on `ErrBudgetExceeded` (RFC §6.15 notes the pause
  hook as a configurable future composition; the V1.1.x behaviour is the
  typed sentinel error + the `governance.*` event, fail-loud).
- No Console governance-enforcement UI changes (the posture page already
  renders tiers; enforcement events ride the existing events surface).

## SDK-consumer reachability

The headless question the audit asks — *can I reach this without the binary?* —
gets a two-level answer:

- **Zero-ceremony (binary + devstack):** populate `governance.identity_tiers`;
  the assembly calls `SetFactory`; `llm.Open` composes enforcement
  automatically. Nothing else to wire.
- **Headless Go consumer:** call
  `governance.NewSubsystemFromConfig(cfg, store, bus)` directly and compose
  with the exported `governance.Wrap(client, sub)` — no `SetFactory`, no
  process-global state, N runtimes per process each with their own tiers.
  This is the documented multi-runtime path (see Goals) and the recipe
  pointer lands in the same PR (a short "enforce governance headless" section
  in `docs/recipes/configure-a-planner.md`'s sibling — see Files).

## Acceptance criteria

- [ ] `governance.NewSubsystemFromConfig(cfg, store, bus)` exported; composes
      MaxTokens → RateLimiter → CostAccumulator via `NewCompound` (the
      documented order); returns `(nil, nil)` on empty `IdentityTiers`
      (D-044 latent default pinned by a unit test); returns wrapped
      `ErrInvalidConfig` on nil store/bus with non-empty tiers.
- [ ] **§13 primitive-with-consumer:** the production assembly (110d's
      merged `assemble.Assemble` — D-197) calls `governance.SetFactory`
      before `llm.Open` whenever `IdentityTiers` is non-empty —
      `SetFactory` gains its first production caller in the same phase
      that ships the assembly helper.
- [ ] The factory consumes the exported config projection
      (`governance.ConfigFromOperator`, from 110c or promoted here); the
      unexported `governanceConfigFromConfig` / `governanceConfigForDevstack`
      duplicates are deleted once both sides converge.
- [ ] E2E (live stack, mock LLM driver): a tier with
      `BudgetCeilingUSD` exhausted → next `Complete` fails with
      `ErrBudgetExceeded` + `governance.budget_exceeded` on the bus; a tier
      with a 1-call rate bucket → second call fails `ErrRateLimited` +
      `governance.rate_limited`; a tier with `MaxTokens: N` → an over-N
      request fails `ErrMaxTokensExceeded` + `governance.maxtokens_exceeded`.
      All three asserted in one integration test against real drivers.
- [ ] Empty/absent `identity_tiers` → byte-identical behaviour to today
      (no wrapper in the chain; golden pass-through test).
- [ ] The Wave A posture-only boot warning is removed; `validateGovernance`
      no longer carries a "not enforced" caveat.
- [ ] Identity isolation: one session exhausting its budget does NOT gate a
      sibling session's calls (cross-session isolation test, §6 rule 10).
- [ ] `governance.Wrap` + `NewSubsystemFromConfig` documented as the headless
      multi-runtime path (godoc + recipe section); the `SetFactory`
      multi-runtime limitation documented on `SetFactory`'s godoc.
- [ ] `scripts/smoke/phase-111a.sh` asserts the enforcement surface (see
      Smoke script additions).
- [ ] D-198 (reserved; logged when the phase ships) records: enforcement
      assembly shape, the SetFactory-vs-per-Open decision, the Wrap escape.

## Files added or changed

- `internal/governance/governance.go` (or `assembly.go`) —
  `NewSubsystemFromConfig` + tests.
- `internal/governance/registry.go` — `SetFactory` godoc: multi-runtime
  limitation + the `Wrap` escape pointer.
- `internal/runtime/assemble/assemble.go` — `SetFactory` call (the merged
  110d assembly site — D-197; cmd + devstack inherit it as thin callers).
- `cmd/harbor/cmd_dev.go` — posture provider keeps its existing wiring;
  duplicate config projection deleted (110c convergence).
- `internal/config/validate.go` — remove the Wave A posture-only warning.
- `test/integration/phase111a_governance_test.go` — the three-enforcer E2E +
  cross-session isolation.
- `docs/recipes/run-harbor-dev.md` (or the recipe the implementor judges
  closest) — short "governance tiers now enforce" note + headless
  `Wrap` snippet.
- `examples/harbor.yaml` — comment on the `governance.identity_tiers` block
  flips from posture-only phrasing to enforcement phrasing.
- `scripts/smoke/phase-111a.sh` — real assertions.
- `docs/decisions.md` — D-198 (reserved; logged when the phase ships).
- `docs/plans/README.md` — status flip on ship.

## Public API surface

- `governance.NewSubsystemFromConfig(cfg Config, store state.StateStore, bus events.EventBus) (Subsystem, error)`.
- `governance.Wrap(inner llm.LLMClient, sub Subsystem) llm.LLMClient` —
  pre-existing, now documented as the headless/multi-runtime composition path.
- `governance.ConfigFromOperator(cfg config.GovernanceConfig) governance.Config`
  — consumed from 110c (or promoted here under the same name).

> Scope note: "public" here is module-internal — `internal/` packages are not
> importable by external modules (the recorded reason `harbortest/` lives at
> the top level). This surface is stable for in-module consumers (cmd,
> harbortest, examples); external-team embedding needs the future facade/export
> RFC (audit §5 / Wave D), out of scope for this phase.

## Test plan

- **Unit:** `NewSubsystemFromConfig` composition order (asserted via a probe
  Subsystem ordering check or the Compound's member order); empty-tiers →
  `(nil, nil)`; nil store/bus + non-empty tiers → `ErrInvalidConfig`;
  factory closure builds per-`llm.Open` (two Opens → two Subsystems, no
  shared accumulator state unless same store keys).
- **Integration:** `test/integration/phase111a_governance_test.go` — real
  drivers (state inmem/sqlite, events inmem, mock LLM via the dev escape
  hatch), full assembly boot, the three-enforcer gating E2E from the
  acceptance criteria, ≥1 failure mode (missing identity → fail-closed
  `ErrIdentityRequired` per `wrap.go`'s contract), identity propagation
  asserted on the emitted `governance.*` events.
- **Conformance:** existing governance conformance suite unchanged; runs
  against the assembly-constructed Subsystem to prove parity.
- **Concurrency / leak:** N≥100 concurrent `Complete` calls through one
  wrapped client under `-race` (the wrapped client + Compound are compiled
  artifacts, D-025); cross-session no-bleed under concurrency; goroutine
  baseline restored after `Close`.

## Smoke script additions

`scripts/smoke/phase-111a.sh` (reclassified to `unit-tests` or `live-server`
as the implementor lands it):

- Static: `governance.SetFactory` has a non-test caller under `cmd/` or the
  promoted assembly (grep gate — the audit's exact regression check).
- `go test ./internal/governance/... ./test/integration/ -run
  'Governance|Phase111a'` green.
- Live (when classified live-server): boot with a 1-call rate-limit tier
  fixture + the mock-LLM escape hatch; second `start` surfaces the
  `governance.rate_limited` event on the events stream. 404/405/501 → SKIP
  pre-phase.

## Coverage target

- `internal/governance`: 90% (the package already carries 85%+; the assembly
  helper is small and fully testable).
- Touched `cmd/harbor` paths: covered via the integration test (cmd is not
  unit-coverage-gated).

## Dependencies

- 32 (LLM client + `llm.Open` chain), 36a (cost accumulator), 36b (rate
  limit + MaxTokens).
- 110c (soft — `governance.ConfigFromOperator`; this phase promotes it itself
  if 110c hasn't merged).
- The in-flight Wave A chore (PR #278) — this phase removes
  the posture-only warning it adds; sequencing is tolerant (if Wave A hasn't
  merged, there is simply no warning to remove).

## Risks / open questions

- **Staging note (Wave C):** the 111 band parallelizes freely once Wave B
  Stage 1 (110a + 110c) merges; 111a soft-depends on 110c only; all six
  111-band phases are mutually independent.
- **Double-enforcement with bifrost-level keys:** bifrost does its own
  multi-key load balancing; governance sits above it and is identity-scoped —
  no overlap, but the integration test should pin that a governance reject
  emits NO provider-side request (the mock driver records zero calls).
- **Factory error behaviour:** `registry.go`'s wrapper currently falls back
  to pass-through on factory error ("rather than fail …"). With a production
  factory installed, a construction failure is an operator misconfiguration —
  the assembly calls `NewSubsystemFromConfig` EAGERLY at boot (fail the boot,
  §13 fail-loud) and hands the already-built Subsystem to the factory
  closure, so the in-wrapper error path stays unreachable in production. The
  implementor verifies this shape against the wrapper hook's contract.
- **110d in-flight:** if 110d merges mid-implementation, the wiring moves to
  `Assemble`; the §17.6 rule (fix both sides) covers the transition.

## Glossary additions

- **Governance enforcement assembly** — the boot-time composition
  (`NewSubsystemFromConfig` → `SetFactory` → `llm.Open` wrapper) that turns a
  populated `governance.identity_tiers` map into live PreCall/PostCall
  enforcement. Add to `docs/glossary.md`.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Cross-session isolation test passes (budget/rate state never bleeds
      across sessions)
- [ ] **Primitive + consumer in the same wave (§13):** `SetFactory` +
      `NewSubsystemFromConfig` ship WITH their production-assembly caller and
      the three-enforcer E2E — checked.
- [ ] Concurrent-reuse test passes (wrapped client + Compound, N≥100, `-race`)
- [ ] Integration test wires real drivers end-to-end, asserts identity
      propagation, covers ≥1 failure mode, runs under `-race`
- [ ] Glossary updated
- [ ] D-198 filed when the phase ships
