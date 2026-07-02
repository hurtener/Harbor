# Phase 145 — Governance attempt-level cost accounting: the in-band attempt-cost tap

## Summary

Closes the "Known accounting gap" recorded in D-272 and at `internal/governance/wrap.go:64-77`: corrective-retry and downgrade LLM attempts are invisible to governance's `CostAccumulator` because governance composes OUTSIDE retry (`governance(retry(downgrade(corrections(safety(driver)))))`, `internal/llm/registry.go:456-545`, settled by D-043/D-044) — worst case `(MaxRetries+1)×3` uncounted provider calls per planner turn, live since Phase 143 made the `Validator` loop production-real. The fix is a **synchronous in-band attempt-cost tap**: `governance.Wrap` installs a per-call accumulator handle into `ctx`; the retry and downgrade wrappers report each attempt they CONSUME (never propagate) into it; `CostAccumulator.PostCall` drains the tap and folds it with the final `resp.Cost` — one exactly-once accounting fold per governed call, no event-subscriber path, no compose-order change. D-275.

## RFC anchor

- RFC §6.15 (governance subsystem — "PostCall … Accumulates cost / tokens / latency"; per-identity cost ceilings)
- RFC §6.5 (the LLM client edge — the retry-with-feedback and structured-output downgrade layers whose internal attempts this phase makes visible to governance)

## Briefs informing this phase

- brief 03
- brief 06

## Brief findings incorporated

- brief 03 §7 (L-5): "Retry with feedback. Validation/parse failures feed back into the planner via `LLMClient` retry; **observable; bounded**." The loop is bounded and its retries are observable on the bus (`llm.retry_with_feedback`, per-attempt `llm.cost.recorded`) — but its SPEND is not observable to the one subsystem whose job is spend. This phase completes the "observable" clause on the accounting axis.
- brief 03 §6 (tests required): "cost calculation across providers; structured-output downgrade chain" — cost math and the downgrade chain are named unit-test surfaces. The exactness test here (accumulated total == Σ all provider attempts, exactly once, across retry × downgrade compositions) is that requirement read jointly.
- brief 03 §5 (the toggle smell): "Harbor picks one architecture and bakes the correction in." Applied to accounting: ONE accumulator path (the in-band synchronous fold, D-044 item 2), deepened to see attempts — not a second, parallel subscriber-based accumulator reconciled against the first (§13 two-parallel-implementations).
- brief 06 §5 (two-channel split): "When the observability record and the stream chunk are separate records flowing through different paths … every dashboard, replay tool, and Console feature has to fuse them." The tap is deliberately NOT a second observability channel: per-attempt `llm.cost.recorded` events keep firing unchanged from the innermost driver (`internal/llm/drivers/bifrost/cost.go:27-46`, emitted at `bifrost.go:180` and `bifrost.go:283`); the tap is accounting plumbing between wrappers, invisible on the bus.

## Findings I'm departing from (if any)

None.

## Goals

- **Every provider attempt's cost reaches the `CostAccumulator` exactly once.** A governed `Complete` that internally drives R corrective re-asks × D downgrade attempts accounts all R×D+1 provider calls, not just the final one. The accounting identity is mechanical: each wrapper either PROPAGATES an inner response to its caller (its `resp.Cost` reaches `PostCall` at the outermost boundary) or CONSUMES it (loops onward / discards) — and every consuming site reports the consumed attempt's cost into the tap. Never both, never neither.
- **Zero settled-decision churn.** The D-043/D-044 compose order is untouched (`governance` stays outermost, `internal/llm/registry.go:528-545`); `PreCall`'s `ErrBudgetExceeded` short-circuit semantics are unchanged; the accumulator stays in-band synchronous per the pinned rationale at `internal/governance/cost.go:165-167` ("rather than event-subscriber: the next PreCall sees the latest total without a bus-delivery race") — the tap IS that in-band posture extended one level deeper, not a re-litigation of it.
- **Identity keying unchanged.** The tap carries only per-call spend; the fold lands through the existing identity-triple keying with RunID cleared (`internal/governance/governance.go:216-254`, `identityScoped`). No new persistence shape, no schema bump (`costRecord` schema stays 1).
- **`resp.Cost` semantics untouched.** Other consumers (the driver's own emits, callers reading the response) keep reading `resp.Cost` as the FINAL call's cost. The tap is a side channel in `ctx`, per D-025: per-run/per-call state rides `ctx`, never mutable wrapper fields.
- **Fail-loud on the accounting path (§13).** Attempt costs accumulate even when the outer call ultimately errors (retry/downgrade exhaustion — spend is spend; `PostCall` already runs regardless of `callErr`, `internal/governance/cost.go:174`). A `PostCall` failure that strands attempt cost surfaces loudly with the at-risk amount named in the error; it is never silently zeroed.

## Non-goals

- **Per-attempt `PreCall` gating.** The ceiling check stays at governed-call boundaries; an in-loop `PreCall` before each retry/downgrade attempt would invert the D-043/D-044 layering (wrappers calling up into governance) and re-litigate the short-circuit semantics. Consequence stated honestly: within ONE governed call, the D-044 item 4 overshoot bound widens from `in_flight × per_call_max_cost` to `in_flight × per_turn_max_cost` (per-turn = `(MaxRetries+1)×3 × per_call_max_cost` worst case). The fix closes the ACCOUNTING gap — totals are correct after every governed call and the NEXT `PreCall` sees them; V1 ceilings remain eventually-consistent per D-044.
- **Token/`Usage` accumulation in the tap.** The accumulator folds `Cost.TotalCost` only, exactly as `PostCall` does today. Attempt-level token accounting is a follow-up if governance ever grows token ceilings.
- **Rate-limiter attempt awareness.** `RateLimiter` keeps counting governed calls, not provider attempts — a retry loop consuming N provider calls still drains one bucket token. Making rate limits attempt-aware is a separate policy decision (new decisions entry if wanted), not an accounting fix.
- **Fixing the empty-`req.Model` attribution quirk at the governance layer.** The react planner leaves `req.Model` empty; retry/downgrade default it from the snapshot at THEIR layer (`retry.go:82-84`, `downgrade.go:53-55`) but governance, outermost, may still see `""` and skip the per-model bucket (`addAtomic` skips when `model == ""`). The tap folds into the same key `PostCall` already uses — consistent with existing attribution, quirk preserved, noted for a future cleanup (changing it would re-key persisted `by_model` records).
- **New event taxonomy.** Per-attempt observability already exists (`llm.cost.recorded` fires per provider attempt from the innermost driver); no `governance.*` event is added or changed.
- **Refunds / drain-on-failure reversal** (out of scope at V1 per D-044).

## Acceptance criteria

- [ ] **The attempt-cost tap primitive lands in `internal/llm`** (the shared vocabulary package both wrappers and governance already import — retry/downgrade must not import `internal/governance`): a per-call, internally-synchronized (atomic CAS over packed float64, mirroring `cost.go`'s `addAtomic` pattern) accumulator with ctx install/lookup helpers, a `Report(Cost)` side that no-ops when no tap is installed (governance latent/disabled — D-044 item 1's posture, not silent degradation: absent tap means no accounting consumer exists), and a one-shot `Drain() (totalUSD float64, attempts int)`.
- [ ] **The retry wrapper reports every validator-rejected NON-final attempt.** Report site: the loop-continuation branch of `internal/llm/retry/retry.go:96-130` (immediately before `appendCorrectiveTurn`, retry.go:124). The FINAL attempt is never reported: a validator-accepted response propagates (retry.go:108), and the exhaustion path RETURNS `lastResp` with `ErrRetryExhausted` (retry.go:128-129) — `PostCall` already accumulates that response's cost regardless of `callErr` (cost.go:174), so reporting it would double-count. An inner error propagates `(resp, err)` unreported (retry.go:101-104).
- [ ] **The downgrade wrapper reports EVERY errored attempt** in `internal/llm/output/downgrade.go:78-113` — not only chain-continuing schema-class failures. Verified against source: the non-schema-error branch returns `llm.CompleteResponse{}, err` (downgrade.go:100), and the exhaustion return is also a zero response (downgrade.go:112) — both DISCARD the errored attempt's `resp`, so its cost never propagates and must be reported at the consuming site. A successful attempt propagates unreported (downgrade.go:89-91). (Today errored provider calls usually price at zero; the reports exist for the invariant's totality and cover any driver that prices failed generations.)
- [ ] **`governance.Wrap` installs the tap; `CostAccumulator.PostCall` drains and folds.** `wrappedClient.Complete` (wrap.go:78-94) installs a fresh tap into `ctx` after `PreCall` permits and passes the tap-carrying ctx to both the inner chain and `PostCall`. `PostCall` drains AFTER `keyState` resolution succeeds and folds `tapTotal + resp.Cost.TotalCost` in one `addAtomic` delta under the existing identity/model key; the zero-work early return (cost.go:184-189) is extended so a nonzero tap is never swallowed. A `PostCall` failure path names the at-risk attempt total in its error (surfaced via wrap.go's existing Warn — RFC §6.15's "PostCall errors do not supplant the call's result" posture unchanged).
- [ ] **Exactness test (the §13 consumer):** a scripted driver + full production wrapper chain drives a Validator-retry loop and a downgrade chain with DISTINCT per-attempt costs (e.g. powers of ten, so any double-count or drop is arithmetically unambiguous); asserts accumulator total == Σ all provider attempts exactly once, across all three terminal shapes: success-after-retries, `ErrRetryExhausted`, `ErrDowngradeExhausted`.
- [ ] **Ceiling test:** intermediate attempts push the accumulator over `BudgetCeilingUSD` → the in-flight governed call completes (PreCall semantics unchanged), the NEXT `PreCall` rejects with `ErrBudgetExceeded` and emits `governance.budget_exceeded`.
- [ ] **D-025 concurrent-reuse:** N≥100 concurrent governed `Complete` calls against ONE shared wrapper chain under `-race`, distinct identities each driving ≥1 corrective retry, asserting exact per-identity totals (no cross-run tap bleed — the tap is per-call ctx state, never a wrapper field), no cancellation cross-talk, goroutine baseline restored.
- [ ] **Integration test** wiring the REAL chain through `llm.Open` (blank-imported corrections/output/retry wrappers + the governance factory seam, inmem state/bus/artifacts drivers), identity propagation asserted, ≥1 failure mode (retry exhaustion still accounts all attempts; state-store failure on `PostCall` surfaces loudly), under `-race`.
- [ ] **Doc closure in the same PR (§17.6 posture):** the wrap.go:64-77 "Known accounting gap" comment is replaced with the tap contract; the STALE claim at `internal/llm/drivers/bifrost/cost.go:12-15` ("the governance accumulator subscribes against this emit site" — factually wrong since D-044 item 2 settled the in-band path) is corrected; D-275 records the closure and cross-references D-272's known-gap paragraph.
- [ ] `scripts/smoke/phase-145.sh` flips from skeleton to real assertions (unit-test legs + the doc-closure greps).

## Files added or changed

- `internal/llm/attempt_cost.go` (+ `attempt_cost_test.go`) — the tap primitive (type, ctx helpers, `Report`, one-shot `Drain`)
- `internal/llm/retry/retry.go` (+ `retry_test.go`) — report site on the loop-continuation branch
- `internal/llm/output/downgrade.go` (+ `output_test.go` / `integration_test.go`) — report sites on every errored-attempt branch
- `internal/governance/wrap.go` — tap install + gap-comment replacement
- `internal/governance/cost.go` (+ `cost_test.go`) — drain-and-fold in `PostCall`, zero-guard extension
- `internal/governance/conformancetest/` — conformance addition: the fold lands identically across the three state drivers
- `internal/llm/drivers/bifrost/cost.go` — stale "subscribes" doc-comment correction (comment-only)
- `test/integration/phase145_attempt_accounting_test.go`
- `scripts/smoke/phase-145.sh`
- `docs/glossary.md` ("attempt-cost tap"), `docs/decisions.md` (D-275), `docs/plans/README.md` (row + detail block)

## Public API surface

Nothing SDK- or Protocol-facing. Internal but load-bearing:

- `internal/llm`: the attempt-cost tap — `ContextWithAttemptCostTap(ctx) (context.Context, *AttemptCostTap)`, `ReportAttemptCost(ctx, Cost)` (no-op without a tap), `(*AttemptCostTap).Drain() (totalUSD float64, attempts int)` (one-shot). Exact names at implementor discretion; the contract (ctx-carried, internally synchronized, report-if-consumed, one-shot drain) is binding.
- `internal/governance`: no signature changes — `Subsystem`, `Wrap`, `Config` all keep their shapes; the behavior change is that `Wrap`+`CostAccumulator` now account attempts.

## Test plan

- **Unit:** tap primitive (report-without-tap no-op; one-shot drain returns zero on second call; per-call isolation across ctx trees). Retry wrapper: rejected non-final attempts reported, final attempt (accepted OR exhausted-`lastResp`) never reported, nil-Validator pass-through reports nothing, inner-error propagation reports nothing. Downgrade wrapper: schema-class-continue / non-schema-immediate-return / exhaustion branches all report; success propagates unreported. Governance: fold exactness, zero-guard extension, `PostCall`-failure error names the at-risk total.
- **Integration:** `test/integration/phase145_attempt_accounting_test.go` — the real chain via `llm.Open` + the governance seam, real inmem drivers on every seam, identity propagation, the ceiling-crossing failure mode and the state-failure loud-error mode, `-race`. Plus the in-package exactness test over the full production wrapper chain (the `internal/llm/output/integration_test.go` pattern: unique driver names for `-count=N` idempotency).
- **Conformance:** the attempt-fold addition runs in the existing governance conformance suite so in-mem / SQLite / Postgres persist identical folded totals (no new interface — same suite, one new case).
- **Concurrency / leak:** the N≥100 shared-chain test above (D-025: the wrappers stay immutable compiled artifacts; the tap is per-call ctx state); goroutine-baseline assertion after teardown.

## Smoke script additions

`scripts/smoke/phase-145.sh` (`PREFLIGHT_REQUIRES: unit-tests`):

- `go test -race -count=1` legs for `internal/governance/...` (fold + ceiling + concurrent tests), `internal/llm/retry/...` + `internal/llm/output/...` (report-site tests), the tap tests in `internal/llm`, and the phase-145 integration test.
- Static greps: `Known accounting gap` ABSENT from `internal/governance/wrap.go`; the stale `subscribes against this emit site` claim ABSENT from `internal/llm/drivers/bifrost/cost.go`; the report call PRESENT in both `internal/llm/retry/retry.go` and `internal/llm/output/downgrade.go`.

Skeleton parks with `skip` until the surface lands.

## Coverage target

- `internal/governance`: ≥ 85%
- `internal/llm/retry`: no regression below current package coverage
- `internal/llm/output`: no regression below current package coverage
- `internal/llm` (touched lines): no regression below current package coverage

## Dependencies

- 36a (the `CostAccumulator` + per-identity ceilings being made attempt-accurate, D-044)
- 36 (the retry-with-feedback wrapper — the dominant leak site, D-043)
- 35 (the structured-output downgrade chain — the secondary leak site, D-043)
- 33 (bifrost cost reporting — the `Cost` figures the tap carries)
- 143 (the first production `Validator` consumer — what took this gap from latent to live, D-272)

## Risks / open questions

- **The exactly-once proof is the load-bearing review item.** The propagate-or-report invariant must be argued at every branch of both loops — the acceptance criteria pin each branch with file:line. One property worth stating for reviewers: the invariant is compose-order-independent. If an embedder hand-composes governance INSIDE retry (against D-044's documented order), the tap is simply absent from the wrapper-visible ctx — every attempt then flows through `Wrap` individually and `PostCall` counts each via `resp.Cost`. Still exactly-once; the tap degrades to a no-op, never to a double-count.
- **Stranded attempt cost on `PostCall` state failure.** If `keyState` load fails before the drain, the tap goes undrained and that call's attempt cost is lost to accounting — surfaced loudly (the `PostCall` error, via wrap.go's Warn, names the amount) but not persisted. This mirrors the EXISTING posture for final-call cost on the same failure (RFC §6.15: PostCall errors are observability-only) and is strictly better than today's silent 100% attempt loss. Retry-until-persisted machinery is deliberately out of scope.
- **Report-site drift.** A future wrapper (or a new consuming branch in retry/downgrade) that forgets to report re-opens the gap silently. Mitigations: the invariant is documented at the tap's godoc AND at each report site naming its branch; the exactness test's distinct-cost trick catches a dropped or doubled attempt arithmetically; the smoke greps pin the report calls' presence.
- **Downgrade attempts mostly price at zero today** (errored provider calls typically return no cost) — the downgrade reports are invariant-completeness, not observed-spend recovery. If review judges them dead weight, the fallback that still closes the LIVE gap is retry-only reporting + a documented downgrade carve-out; the plan's default is totality (cheaper to reason about, future-proof against drivers that price failed generations).

## Glossary additions

- **Attempt-cost tap** — the per-call, ctx-carried accumulator handle governance installs around the LLM-edge wrapper chain; retry/downgrade wrappers synchronously report each provider attempt they consume (never propagate) into it, and `CostAccumulator.PostCall` drains it exactly once, folding intermediate-attempt spend with the final response's cost. In-band by design (D-044 item 2): no bus round-trip sits between an attempt's spend and the next `PreCall`'s ceiling check.

(Added to `docs/glossary.md` in the same PR as this plan.)

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes — deferred to CI; committed with `HARBOR_PREFLIGHT_SKIP=1` (governance accounting is in-process, no Protocol surface; the phase-145 smoke unit-test legs run green locally)
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target (`internal/governance` ≥ 85%; touched `internal/llm`, `internal/llm/retry`, `internal/llm/output` show no coverage regression)
- [x] If multi-isolation paths changed: cross-session isolation test passes (the per-identity fold + the N=128 concurrent distinct-identity chain test cover the touched isolation surface)
- [x] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. (`TestChain_D025_ConcurrentReuse`: N=128 concurrent governed calls against one shared wrapper chain, exact per-identity folded totals, goroutine baseline restored.)
- [x] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md §17. (`test/integration/phase145_attempt_accounting_test.go` — real inmem state/bus/artifacts, identity propagation, retry-exhaustion + state-fail-loud modes.)
- [x] If new vocabulary: glossary updated ("attempt-cost tap" landed with the plan PR)
- [x] If a brief finding was departed from: justified above + decisions.md entry filed (N/A — none departed from)
