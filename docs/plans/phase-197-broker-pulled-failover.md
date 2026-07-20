# Phase 197 — broker-pulled Harbor-orchestrated failover chains (D-335) + v1.17 wave-end E2E

## Summary

Realizes the governance `FailoverPolicy` seam (the post-V1 phase-93 slot)
over a broker-pulled ORDERED chain of provider keys (D-333): on a retryable
provider error the policy advances to the next key/provider, emits a
`governance.failover` hop event (cost + identity attached), **re-runs
Governance `PreCall` (budget / rate-limit / MaxTokens) BEFORE re-issuing**
— not merely PostCall accounting — and re-issues through the one-method
`LLMClient`. D-018 stands: bifrost's native `Fallbacks` array is NOT used —
every hop stays a Harbor event through the audit + bus + per-identity cost
accumulator. Cross-provider (heterogeneous) chains are fully expressible;
fallback keys are themselves broker-pulled (D-333) and never persisted. A
hop that trips `PreCall` fails loud (`ErrBudgetExceeded` / `ErrRateLimited`)
and does NOT silently continue down the chain. This phase also bundles the
v1.17 wave-end E2E (`test/integration/wave_v117_test.go`) per §17.7 step 5.

## RFC anchor

- RFC §6.15
- RFC §6.5

## Briefs informing this phase

- brief 03
- brief 08

## Brief findings incorporated

- brief 08 §"bifrost exposes a per-request `Fallbacks` array — Harbor does
  NOT use it": using it would hide every fallback hop from Harbor's audit
  redactor, event bus, and per-identity cost accumulator. This phase walks
  the chain at the Governance layer instead (D-018 / D-335), so every hop is
  a Harbor `governance.failover` event with cost + identity — the mechanism
  is Harbor-orchestrated even though the capability mirrors what the SDK
  offers.
- brief 08 §"`Account.GetKeysForProvider` is invoked per request; keys can
  change without `ReloadConfig`": the fallback provider's key is
  broker-pulled (D-333, Phase 196) and installed on the LiveKey atomic swap,
  so advancing to the next key/provider in the chain is a swap + re-issue,
  not a `ReloadConfig` race.
- brief 03 §"The LLM client is one method; the runtime owns orchestration":
  failover re-issues through the SAME `LLMClient.Complete` — the chain walk
  is runtime/governance mechanism ABOVE the one-method client, not a new
  client method or a `CompleteRequest.Fallbacks` field.
- brief 03 §"Cost + rate limits are per-identity and must gate BEFORE the
  call, not just account after": D-335's load-bearing rule — re-run
  `PreCall` before each re-issue, so an N-provider chain cannot push a run
  past its per-identity ceiling across hops (the exact cost-control hole
  D-018 exists to prevent). A PostCall-only chain would silently spend N×
  the ceiling; this phase gates every hop.

## Findings I'm departing from (if any)

None. D-335 explicitly EXTENDS D-018 (does not reverse it): Harbor
orchestrates failover at the Governance layer, bifrost's `Fallbacks` stays
unused. This phase implements exactly that posture. No brief finding is
contradicted.

## Goals

- Implement the governance `FailoverPolicy` seam (RFC §6.15, the post-V1
  phase-93 slot) as a real driver behind the §4.4 seam pattern — a
  primitive shipped WITH its consumer (the LLM-call wrap that walks it),
  per CLAUDE.md §13.
- Walk a broker-pulled ORDERED chain (D-333, Phase 196): each entry a named,
  zero-URL/zero-secret broker descriptor; on a retryable provider error,
  advance to the next key/provider.
- Emit `governance.failover` per hop, with cost + identity attached, through
  the audit + bus + per-identity cost accumulator.
- **Re-run `PreCall` (budget / rate-limit / MaxTokens) BEFORE each
  re-issue** — a hop that trips `PreCall` fails loud (`ErrBudgetExceeded` /
  `ErrRateLimited`) and does NOT silently continue down the chain.
- Re-issue through the same one-method `LLMClient`; the fallback provider's
  key is broker-pulled (D-333) and never persisted.
- Express cross-provider (heterogeneous) chains without delegating
  orchestration to the SDK — D-018 stands, bifrost `Fallbacks` NOT used.
- Bundle the v1.17 wave-end E2E `test/integration/wave_v117_test.go` per
  §17.7 step 5 — real drivers, `-race`, N≥10 concurrency, identity
  propagation, ≥1 failure mode, spanning the wave surfaces.

## Non-goals

- No use of bifrost's `schemas.Fallback` / per-request `Fallbacks` array —
  D-018 / D-335 forbid it.
- No circuit breaker (that is the separate post-V1 phase-94 seam
  `CircuitBreaker`); this phase walks the chain on a retryable error, it
  does not track per-(provider, key) health across runs.
- No persistence of fallback keys — they are broker-pulled per D-333 and
  held in memory only.
- No new `LLMClient` method and no `CompleteRequest.Fallbacks` field — the
  chain walk is governance mechanism above the one-method client.
- No new broker-pull mechanics — this phase CONSUMES Phase 196's
  `InferenceKeySource`; it does not re-implement the pull.
- No change to the retryable-vs-permanent classification beyond wiring the
  existing LLM error classification into the advance decision (a permanent
  error stops the walk, not advances it).
- No new Protocol write — the chain is boot-declared / installed via Phase
  196's `set_llm_provider` (an ordered set of broker descriptors); this
  phase adds only the `governance.failover` EVENT to the wire surface.

## Acceptance criteria

- [ ] A `FailoverPolicy` driver (behind the §4.4 governance seam) walks an
      ORDERED chain of broker-pulled provider descriptors; on a retryable
      provider error from `LLMClient.Complete`, it advances to the next
      entry and re-issues; on a PERMANENT error (or chain exhaustion) it
      fails loud with the terminal error, never a silent success.
- [ ] Before EACH re-issue, the policy re-runs Governance `PreCall`
      (budget / rate-limit / MaxTokens) for the run's identity; a hop that
      trips `PreCall` fails loud (`ErrBudgetExceeded` / `ErrRateLimited`)
      and the walk STOPS — it does not continue down the chain. A test
      proves an N-provider chain cannot push a run past its per-identity
      budget ceiling across hops (the D-018 cost-control hole).
- [ ] Each hop emits a `governance.failover` event (canonical, SafePayload)
      carrying the run identity, the from/to provider, the hop index, and
      the accumulated cost — through the audit Redactor + event bus + the
      per-identity cost accumulator (bifrost `Fallbacks` is NOT used; a
      static assertion / code-review guard confirms `schemas.Fallback` is
      never constructed).
- [ ] The fallback provider's key is broker-pulled via Phase 196's
      `InferenceKeySource` (D-333) and never persisted; advancing a hop swaps
      the LiveKey (D-019) and re-issues through the one-method `LLMClient`.
- [ ] Cross-provider chains are expressible: a chain whose entries name
      DIFFERENT providers (e.g. openai → anthropic) walks correctly, each
      hop re-issuing against the next provider's broker-pulled key, each hop
      a Harbor event.
- [ ] `LLMClient` stays one method; no `CompleteRequest` field added; the
      walk is entirely governance/runtime mechanism above the client.
- [ ] `governance.failover` is added to `internal/protocol/methods` (as an
      event type) / `internal/protocol/types` with full D-223 / D-209
      lockstep; `ProtocolVersion` stays `0.1.0`.
- [ ] `test/integration/wave_v117_test.go` proves, over real drivers, under
      `-race`, N≥10 concurrency, with identity propagation asserted
      throughout: the v1.17 wave surfaces end-to-end —
      (192) a group-cancelled mirror,
      (193) a planner steer / pause / resume round-trip,
      (194) a per-tool binding,
      (195) a `governance.set_posture` write + round-trip read,
      (196) a broker-pull provider install + a fail-loud
      `ErrProviderKeyUnavailable` on an unreachable broker,
      (197) a failover walk with a PreCall-trip (a hop that trips the budget
      ceiling fails loud and stops the walk) — the ≥1 failure mode.
- [ ] `scripts/smoke/phase-197.sh` exercises the `governance.failover` event
      surface where reachable and runs `TestE2E_WaveV117` under `-race` with
      a no-match-fails guard.
- [ ] §18: `use-the-harbor-protocol` and the governance/observability
      skill(s) (grepped per §18) updated in the same PR; docs-site nav +
      generated protocol reference regenerated in the same PR.

## Files added or changed

```text
internal/governance/failover.go                        # FailoverPolicy driver: chain walk, PreCall-before-reissue, hop event (new)
internal/governance/failover_test.go                   # unit: advance-on-retryable, stop-on-permanent, PreCall-trip-fails-loud, cross-provider
internal/governance/wrap.go                            # wire FailoverPolicy into the LLM-call wrap (re-issue path)
internal/governance/events.go                          # GovernanceFailoverPayload (SafePayload: identity, from/to, hop index, cost)
internal/governance/registry.go                        # register the FailoverPolicy driver behind the §4.4 seam
internal/llm/credsource/inference_source.go            # (consume) ordered-chain descriptors → per-hop LiveKey swap (from Phase 196)
internal/protocol/types/governance.go                  # GovernanceFailoverEvent wire type
internal/protocol/methods/methods.go                   # governance.failover event registration
web/console/src/lib/protocol/wire-manifest.gen.json    # regenerated (make protocol-ts-gen)
docs/site/protocol/events.md, methods.md               # regenerated (make protocol-docs-gen)
docs/skills/use-the-harbor-protocol/SKILL.md            # governance.failover event note
docs/skills/<governance/observe skill>/SKILL.md          # failover hop visibility (grep-identified)
docs/glossary.md                                        # 2 new terms
test/integration/wave_v117_test.go                      # wave-end E2E (spans 192–197)
scripts/smoke/phase-197.sh
```

## Public API surface

```go
// internal/governance/failover.go

// FailoverPolicy walks an ordered chain of broker-pulled provider
// descriptors on a retryable provider error, re-running PreCall before each
// re-issue and emitting a governance.failover hop event. It realizes the
// RFC §6.15 FailoverPolicy seam (the post-V1 phase-93 slot) — D-018 stands:
// bifrost's native Fallbacks array is NOT used. Immutable after
// construction; safe to share across N goroutines (per-run state on ctx /
// RunContext, never on the policy — D-025).
type FailoverPolicy interface {
    // Complete issues the request against the primary provider, then walks
    // the ordered chain on a retryable error. Before EACH re-issue it
    // re-runs PreCall for ident; a hop that trips PreCall returns the
    // budget/rate sentinel and STOPS the walk (no silent continue). Each hop
    // emits governance.failover. Returns the first successful response, or
    // the terminal error on chain exhaustion / a permanent error.
    Complete(ctx context.Context, ident Identity, req llm.CompleteRequest, chain []ProviderRef) (llm.CompleteResponse, error)
}

// ProviderRef names one entry in the ordered failover chain — a
// broker-pulled, zero-URL/zero-secret descriptor (D-333); the key is pulled
// via Phase 196's InferenceKeySource and never persisted.
type ProviderRef struct {
    Provider string // may differ across entries (cross-provider chain)
    Name     string // the installed provider binding name (bare-name resolution, D-303)
}
```

```go
// internal/governance/events.go — SafePayload, canonical.

type GovernanceFailoverPayload struct {
    events.SafeSealed
    Identity     identity.Quadruple // run identity — the hop is cost-attributed per-identity
    FromProvider string
    ToProvider   string
    HopIndex     int
    AccumCostUSD float64            // accumulated cost across hops so far (for the ceiling check)
    Reason       string             // the retryable error class that triggered the advance
}
```

## Test plan

- **Unit:**
  - `failover_test.go`: a retryable error on the primary advances to entry 2
    and succeeds (one `governance.failover` emitted); a PERMANENT error stops
    the walk immediately (no advance, terminal error returned); chain
    exhaustion returns the last error loud; a cross-provider chain
    (openai → anthropic) walks and each hop re-issues against the next
    provider; **the PreCall-trip case** — a hop whose re-run `PreCall` trips
    the budget ceiling returns `ErrBudgetExceeded` and STOPS (the next entry
    is never tried), proving the N-hop-cost-hole is closed; a static guard
    asserts `schemas.Fallback` is never constructed anywhere in the walk.
  - `wrap_test.go`: the LLM-call wrap routes through `FailoverPolicy` when a
    chain is configured and through the plain single-provider path when not
    (no two-parallel-implementations toggle — one path, chain length 0 ==
    single provider).
- **Integration:**
  - `test/integration/wave_v117_test.go` (real drivers, `-race`, N≥10
    concurrency): the six-surface proof in Acceptance Criteria, identity
    propagation asserted on every leg, the failover leg driven by a fixture
    LLM/broker that forces a retryable error on the primary and a
    budget-ceiling PreCall-trip on a later hop — the wave's ≥1 failure mode.
    Two tenants run the failover scenario concurrently with no cross-talk
    (per-identity cost accumulators stay isolated across the hop walk).
  - The failover hop event is asserted on the real bus (through the real
    Redactor), keyed by the run identity, cost-attributed.
- **Conformance:** N/A — `FailoverPolicy` is not a persistence-shaped
  subsystem (the cost accumulator it consults IS StateStore-backed and its
  conformance is already covered by the governance suite; this phase adds no
  new persisted state).
- **Concurrency / leak:**
  - `TestConcurrentReuse_FailoverPolicy_NoBleed`: N≥100 concurrent
    `Complete` walks against a single shared `FailoverPolicy` instance under
    `-race` — no data race, no per-run state on the policy (per-run cost /
    hop index lives on ctx/RunContext, D-025), no cross-identity cost bleed
    (run A's accumulated hop cost never gates run B), no goroutine leak after
    teardown.

## Smoke script additions

- `scripts/smoke/phase-197.sh` (`PREFLIGHT_REQUIRES: live-server` +
  `unit-tests`):
  - Static trip-wire: `governance.failover` present in
    `wire-manifest.gen.json` + `docs/site/protocol/events.md` (SKIP pre-197).
  - Where the event surface is reachable on the live server (an events
    subscription / metrics probe), assert the `governance.failover` event
    type is advertised; SKIP (404/405/501) gracefully otherwise.
  - Runs `TestE2E_WaveV117` under `-race` with a no-match-fails guard (the
    wave-end E2E is the load-bearing gate for this phase).

## Coverage target

- `internal/governance` (touched — `failover.go`, `wrap.go`, `events.go`): 85%
- `internal/protocol/types` (touched): 85%

## Dependencies

- 196 (the broker-pull source `InferenceKeySource` + the ordered-chain
  descriptor shape this phase walks; D-333/D-334). 197 follows 196 in
  Stage 3.
- Gate-0 (RFC §6.15 + D-335 — shipped on the branch base).
- 192, 193, 194, 195, 196 — for the bundled v1.17 wave-end E2E only (the
  group-cancelled mirror, planner steer/pause/resume, per-tool binding,
  governance write, and broker-pull surfaces `wave_v117_test.go` exercises
  alongside this phase's own failover-with-PreCall-trip leg). Sibling phases
  192–194 are planned in the same wave; the E2E references their shipped
  surfaces, per the phase-191 → 185–190 precedent.

## Risks / open questions

- **Design choice the decision left for the phase to confirm — where the
  ordered chain is declared.** D-335 says the broker-pull source "supplies
  the ordered chain of keys/providers the consumer configured". D-334's
  `set_llm_provider` installs a single named provider; the CHAIN is an
  ORDERED SET of such installs. Two readings: (a) the chain is declared as an
  ordered list on the boot inference config / an additive ordered field, or
  (b) the chain is assembled from N `set_llm_provider` installs plus a
  separate ordering declaration. **This plan takes (a)** — the ordered chain
  is a boot-declared / installed ordered list of provider-binding names, so
  the WALK ORDER is not itself a wire-writable credential-sink lever (it
  references installed, zero-URL bindings by name). **Flag for the
  coordinator to confirm** whether the chain ordering should itself be
  Protocol-writable (a new `set_failover_chain` write) or stay
  boot/install-declared. D-335 does not mandate a new write, so this plan
  adds none; if the coordinator wants an admin-writable chain order, that is
  a scoped follow-up with its own zero-sink-field analysis.
- **Retryable-vs-permanent classification is load-bearing for the walk.**
  Advancing on a PERMANENT error (e.g. an invalid-request 400) would waste
  the whole chain and mask a real bug; stopping on a genuinely transient
  error would defeat failover. This phase reuses the existing LLM error
  classification (`internal/llm/retry` / the error-class taxonomy) rather
  than inventing a new one — a permanent error stops the walk, a retryable
  error advances it. The test pins both branches.
- **PreCall re-run cost accounting.** The re-run `PreCall` must see the
  ACCUMULATED cost across hops (not reset per hop), or the ceiling check is
  toothless. The `GovernanceFailoverPayload.AccumCostUSD` and the per-run
  cost carried on ctx/RunContext (never on the policy — D-025) are the
  accumulation seam; the integration test asserts an N-hop walk trips the
  ceiling at the right hop, not after N× the budget.
- **Wave-E2E sibling coupling.** `wave_v117_test.go` exercises surfaces from
  192–195 that are planned by sibling agents. If a sibling surface's exact
  method name / shape drifts from this plan's assumption, the E2E is fixed in
  THIS PR (§17.6 — fix what the test finds, across phase boundaries), not
  deferred.

## Glossary additions

- **`governance.failover` (broker-pulled hop)**
- **Harbor-orchestrated failover chain (vs bifrost `Fallbacks`)**

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: the wave E2E's N≥10 concurrency
      across ≥2 tenants asserts per-identity cost-accumulator isolation
      across the hop walk (no cross-identity budget bleed).
- [ ] This phase builds a reusable artifact (`FailoverPolicy`):
      concurrent-reuse test passes — N≥100 concurrent invocations against a
      single shared instance under `-race`, no per-run state on the policy,
      no cross-identity cost bleed.
- [ ] This phase consumes shipped subsystems (Phase 196 broker-pull source,
      governance PreCall / cost accumulator, LLM client, Bus, Redactor):
      integration test (`wave_v117_test.go`) wires real drivers end-to-end,
      asserts identity propagation, covers ≥1 failure mode (PreCall-trip +
      broker-unreachable), runs under `-race`.
- [ ] Glossary updated (2 terms above)
- [ ] N/A — no brief finding departed from (D-335 extends D-018; the plan
      implements that posture exactly)
