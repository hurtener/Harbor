# Phase 36b — Per-identity rate limits + per-call MaxTokens (governance)

## Summary

Ship a token-bucket rate limiter per `(identity, model)` whose state lives
in `state.StateStore` so it survives runtime restart. Add per-call
MaxTokens enforcement resolved from the identity tier in `PreCall`. Both
fail loudly with typed sentinels (`ErrRateLimited`, `ErrMaxTokensExceeded`)
and emit observable events. Builds on Phase 36a's `Subsystem` scaffolding
and inherits its latent-default posture.

## RFC anchor

- RFC §6.15

## Briefs informing this phase

- brief 03
- brief 06

## Brief findings incorporated

- **brief 03 §6:** The bifrost wire surface accepts `MaxTokens` per request
  (mapped through Phase 32's `CompleteRequest.MaxTokens *int`). Phase 36b
  hooks into the existing field in PreCall before the request flows
  through the rest of the LLM chain.
- **brief 06 §3:** Token-bucket emit pattern matches the rest of Harbor's
  governance: the bus event is a SafePayload carrying identity + model +
  the bucket level. Console + Protocol subscribe in later phases.

## Findings I'm departing from (if any)

- None. Phase 36b follows the master plan + RFC §6.15 verbatim; the
  fail-loud-on-MaxTokens semantic (vs clamping) is settled by master plan
  line 420 ("emits `governance.maxtokens_exceeded` events; fails loudly
  with `ErrMaxTokensExceeded`") + RFC §6.15 line 1122 ("PreCall ... returns
  a typed sentinel error to gate the call: ... ErrMaxTokensExceeded").

## Goals

- Token-bucket `RateLimiter` Subsystem with per-`(identity, model)` state
  in StateStore.
- Restart-survival: bucket state reads back on `New` from StateStore.
- `MaxTokensEnforcer` Subsystem checks `req.MaxTokens` against the tier
  cap in `PreCall`; exceedance → `ErrMaxTokensExceeded`.
- Register `governance.rate_limited` + `governance.maxtokens_exceeded`
  events.
- Compose with the Phase 36a `CostAccumulator` via `CompoundSubsystem`.
- Latent default: empty config → permits.
- Three-driver conformance for bucket state.
- Controllable clock for time-based bucket-refill tests.

## Non-goals

- Refunds on call failure (RFC §6.15 simplicity — drain-on-PreCall is
  final).
- Per-tier override of the bucket-refill formula (Phase 36b ships one
  formula: linear refill at `RefillTokens` per `RefillInterval`).
- Burst-only buckets (not a V1 ask — operators wanting bursts set Capacity
  larger than the typical RefillTokens).
- Adaptive rate limits (post-V1).

## Acceptance criteria

- [ ] `RateLimiter` Subsystem implements `PreCall` / `PostCall`. PreCall
      drains `expected_tokens` (from `req.MaxTokens` if set, else
      `ModelProfile.DefaultMaxTokens`, else 1) from the per-key bucket.
- [ ] PreCall on drain underflow returns wrapped `ErrRateLimited` and
      emits `governance.rate_limited`.
- [ ] Bucket state persists across `Close` + reopen.
- [ ] Three-driver conformance (in-mem / SQLite / Postgres) for the
      bucket persistence.
- [ ] **Empty config → no enforcement.** With `TierConfig.RateLimit`
      zero-valued, PreCall is a permit no-op.
- [ ] `MaxTokensEnforcer` PreCall returns `ErrMaxTokensExceeded` when
      `req.MaxTokens > tier.MaxTokens`. Permits when either is zero.
- [ ] Emits `governance.maxtokens_exceeded` on rejection.
- [ ] StateStore read failure on PreCall lookup → wrapped error (no silent
      permit).
- [ ] **High-concurrency gate**: N concurrent calls against ONE bucket
      with small capacity. Bucket never goes negative; never permits more
      than `capacity` calls in a refill window.
- [ ] D-025 concurrent-reuse on `RateLimiter` + `MaxTokensEnforcer`.
- [ ] Identity isolation: bucket A's drain does not affect bucket B's
      level.
- [ ] `scripts/smoke/phase-36b.sh` green.
- [ ] Coverage on `internal/governance`: ≥ 85% (cumulative across 36a +
      36b).

## Files added or changed

- `internal/governance/ratelimit.go` (new) — `RateLimiter` Subsystem.
- `internal/governance/maxtokens.go` (new) — `MaxTokensEnforcer` Subsystem.
- `internal/governance/ratelimit_test.go` (new).
- `internal/governance/maxtokens_test.go` (new).
- `internal/governance/conformancetest/conformancetest.go` (modified) —
  extends the suite with bucket-persistence cases.
- `internal/governance/events.go` (modified) — adds the two new event
  types + payloads.
- `internal/governance/errors.go` (modified) — adds `ErrRateLimited` +
  `ErrMaxTokensExceeded`.
- `internal/governance/registry.go` (modified) — wires the compound
  subsystem so a single registered governance wrapper covers all three
  policies.
- `internal/config/config.go` + `internal/config/validate.go` (modified)
  — `TierConfig.RateLimit` + `TierConfig.MaxTokens` validation.
- `examples/harbor.yaml` (modified) — commented-out sample bucket +
  MaxTokens tier.
- `scripts/smoke/phase-36b.sh` (new) — smoke.
- `README.md` + `docs/plans/README.md` (modified) — Phase 36b row →
  Shipped.

## Public API surface

```go
func NewRateLimiter(state state.StateStore, bus events.EventBus, cfg Config) (*RateLimiter, error)
func NewMaxTokensEnforcer(bus events.EventBus, cfg Config) *MaxTokensEnforcer

type RateLimiter struct { /* ... */ }
type MaxTokensEnforcer struct { /* ... */ }
```

Both satisfy `governance.Subsystem`. Compose under `CompoundSubsystem`
with the Phase 36a `CostAccumulator`.

## Test plan

- **Unit:** Token-bucket math under fast + slow refill. Drain semantics.
  Identity isolation. MaxTokens fail-loud + permit paths. Empty config →
  permits.
- **Integration:** RateLimiter PreCall blocks; bucket survives Close +
  reopen against in-mem StateStore. `CompoundSubsystem` fans out PreCall
  across all three policies and short-circuits on the first failure.
- **Conformance:** Three-driver suite extends to bucket persistence.
- **Concurrency / leak:** N≥100 concurrent PreCalls against one bucket;
  bucket never negative; permits ≤ capacity per refill window. D-025
  reuse.

## Smoke script additions

- `scripts/smoke/phase-36b.sh` exercises the package under `-race` and
  asserts the new event types are registered.

## Coverage target

- `internal/governance`: ≥ 85% (cumulative).

## Dependencies

- 11 (event bus skeleton).
- 15 (StateStore SQLite driver — bucket persistence).
- 36a (Subsystem interface + identity scaffolding).

## Risks / open questions

- **Bucket time math races.** Each PreCall computes the refill delta
  since `LastRefill` then atomically decrements. The drain operation is
  a CAS loop over the bucket's level + last-refill timestamp packed into
  a single atomic value (or guarded by per-key mutex; we pick the simpler
  per-key mutex for V1 since contention is per-identity-per-model and
  per-key mutexes are not a global bottleneck).
- **Write-on-drain to StateStore.** Every PreCall persists the new bucket
  level. This is a hot-path write. Acceptable for V1 because (a) write
  rate is bounded by call rate, (b) the StateStore drivers (in-mem +
  SQLite + Postgres) are designed for it. Batched writes are a future
  optimization tracked here, not implemented now.

## Glossary additions

- `TokenBucket`
- `RateLimit`
- `MaxTokens` (governance sense)
- `MaxTokensEnforcer`
- `Clock` (governance sense)

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Cross-session isolation test passes
- [ ] Concurrent-reuse test passes — N≥100 invocations under `-race`
- [ ] Integration test wires real drivers; failure-mode coverage
- [ ] Glossary updated
