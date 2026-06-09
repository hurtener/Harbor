# Phase 111e — Trajectory compression consumer

## Summary

Trajectory compression is Harbor's "durable long-running agents" value prop —
and it is dead on every production path. `planner.Summariser`
(`internal/planner/compression.go:36-44`) has only test implementations;
`CompressionRunner.MaybeCompress` (`compression.go:135` constructor,
`compression.go:187` entrypoint) has **zero call sites**; `Budget.TokenBudget`
(`internal/planner/planner.go:563-573`) is a dead field whose godoc promises
runtime behaviour ("the runtime's CompressionRunner reads this field") that no
runtime performs — the production `RunSpec` never sets `Budget`
(`cmd/harbor/cmd_dev_runloop.go:681-702`). The consumer side, notably, is
ALREADY wired: the React prompt builder renders `Trajectory.Summary` when
non-nil and skips the per-step history (`internal/planner/react/prompt.go:38-47,221-259`)
— the moment compression fires, the prompt shrinks. Only the producer chain is
missing.

One trap the audit pinned explicitly: `internal/llm/summarizer` implements the
UNRELATED `memory.Summarizer` interface (different signature, Phase 64/D-089's
memory-strategy summarizer) — **do not conflate**. Phase 111e ships a real
LLM-backed `planner.Summariser`, the `MaybeCompress` integration point in
`steering.RunLoop`'s step loop gated on `Budget.TokenBudget > 0`, the
production `RunSpec`/`Budget` wiring from a new config knob, and un-dormants
the godocs the in-flight Wave A chore (branch `chore/sdk-audit-wave-a`) marks
dormant. Recommendation: **ship, not defer** — scoped tight (single
compression per run, no auto-cascade).

## RFC anchor

- RFC §6.2 — Planner interface, Trajectory, RunContext (`Budget` as a
  runtime-level run option; `Trajectory.Summary` as the compaction artefact
  the planner sees).
- RFC §6.5 — LLM client layer (the `LLMClient.Complete` the summariser
  invokes; the D-026 context-window safety net compression keeps the
  trajectory under).

## Briefs informing this phase

- brief 02 — planner + steering + HITL (trajectory compression's designed
  shape: the runner, the five-field summary, the runtime-invoked summariser)

## Brief findings incorporated

- **brief 02 §trajectory compression.** "When `token_budget` is exceeded, the
  runtime invokes a configurable summariser … the compressed digest replaces
  raw step history in subsequent prompt builds; the planner sees the
  compressed view via `RunContext.Trajectory.Summary`." This is implemented
  end-to-end EXCEPT the "runtime invokes" clause — this phase is that clause.
- **brief 02 §runtime loop pseudocode.** "`if token_budget exceeded: invoke
  summarizer`" sits INSIDE the step loop, after observation append and before
  the next planner step — pinning the integration point this phase wires into
  `steering.RunLoop`.
- **brief 02 §planner knobs.** "Token budget, hop budget, deadline … are
  runtime-level run options, not planner state" — the wiring goes through
  `RunSpec.Base.Budget` (per-run), never onto the planner struct (D-025).

## Findings I'm departing from (if any)

None.

## Goals

- **Real LLM-backed `planner.Summariser`.** New constructor in
  `internal/llm/summarizer` — `NewTrajectorySummariser(client llm.LLMClient, opts ...TrajectoryOption) (*TrajectorySummariser, error)`
  — a DISTINCT type alongside the memory `Summarizer`, with package godoc
  spelling out the two-interface split (`memory.Summarizer`: conversation
  window → summary text; `planner.Summariser`: trajectory → five-field
  `TrajectorySummary`). Import direction is clean
  (`llm/summarizer` → `planner` → `llm`; no cycle) and the home follows the
  Phase 64/D-089 precedent of production LLM-backed summarization living
  there. The implementation composes a compaction prompt over
  `Trajectory`'s serialized state, calls `Complete` (structured-output
  JSON-schema mode for the five fields, with the existing downgrade ladder),
  and parses into `TrajectorySummary` — failing loud per the seam's contract
  (`ErrEmptySummary` on a vacuous result; errors propagate verbatim).
- **RunLoop integration point.** `steering.RunSpec` gains a
  `Compression *planner.CompressionRunner` field (nil = no compression —
  byte-identical behaviour to today). At the top of each step (after the
  control drain, before `Planner.Next`), when
  `rc.Budget.TokenBudget > 0 && spec.Compression != nil`, the RunLoop calls
  `MaybeCompress(ctx, rc, tr)`. The runner's existing semantics hold: token
  estimate under budget → no-op; over budget → summarise + stamp
  `Trajectory.Summary` + emit `trajectory.compressed`; idempotent on
  `Summary != nil` (single compression per run at V1.1.x — the documented
  tight scope; re-compaction cadence is the recorded follow-up, no
  auto-cascade).
- **Production wiring.** New config knob `planner.token_budget` (int; 0 =
  disabled, today's behaviour, the default) → validated → projected onto
  `RunSpec.Base.Budget.TokenBudget` in the per-task run loop
  (`cmd_dev_runloop.go`) → `bootDevStack` constructs the runner
  (`planner.NewCompressionRunner(summariser)` over a
  `NewTrajectorySummariser(llmClient)`) when the budget is non-zero.
  Devstack D-094 mirror in the same PR. (If Wave B's 110a/110d promotions
  land first, the wiring lands in the promoted run-loop/assembly instead —
  either order works; §17.6 covers the transition.)
- **Godoc honesty, reversed.** `compression.go:36-44`'s "the production
  LLM-backed summariser lands when …" and `planner.go:563-573`'s "the
  runtime's CompressionRunner reads this field" become TRUE; the Wave A
  chore's dormant-seam markers on these sites are removed (its honest
  posture is superseded by the real thing).
- **Long-trajectory E2E.** A scripted multi-step run whose trajectory
  exceeds a small `token_budget`: compression fires mid-run, the NEXT
  prompt build renders the `<trajectory summary>` path instead of per-step
  history (the prompt byte-length drops — asserted), and the run completes
  with a correct final answer that depends on a fact captured in the
  summary (proving the compaction preserved load-bearing context, not just
  shrank bytes).

## Non-goals

- **No auto-cascade / re-compaction loop.** One compression per run at
  V1.1.x (the runner's `Summary != nil` idempotence is the scope fence); a
  trajectory that re-exceeds budget post-compression grows until D-026's
  context-window safety net backstops it. Re-compaction cadence is recorded
  as the follow-up in D-202.
- No memory-subsystem changes — `memory.Summarizer` and its rolling-summary
  strategy are untouched (the non-conflation rule cuts both ways).
- No per-planner-concrete compression policy — the runner is runtime
  mechanism (RFC §6.2's separation); the Deterministic planner simply never
  sets a budget.
- No Console "compression fired" UI — `trajectory.compressed` rides the
  canonical events surface; Console work only if operator feedback asks.
- No new structured-output machinery — the summariser uses the existing
  Phase 35 modes + downgrade ladder.

## SDK-consumer reachability

Every piece is independently constructible headless:
`summarizer.NewTrajectorySummariser(client)` →
`planner.NewCompressionRunner(s)` → set `RunSpec.Compression` +
`Base.Budget.TokenBudget` on `steering.NewRunLoop`'s spec. No config file, no
Protocol, no binary. The config knob is a thin carrier over the programmatic
surface (the 84-band pattern: the policy core is exported; YAML is one
adapter). The headless snippet lands in the
`docs/recipes/configure-a-planner.md` budget section (or the recipe the
implementor judges closest), including the budget-zero-means-off contract.

## Acceptance criteria

- [ ] `summarizer.NewTrajectorySummariser(client, opts...)` ships; satisfies
      `planner.Summariser`; compile-time assertion present; package godoc
      disambiguates the two summarizer interfaces; `ErrEmptySummary` +
      verbatim error propagation honoured (the seam's existing fail-loud
      contract, now under a real implementation).
- [ ] **§13 primitive-with-consumer:** `MaybeCompress` gains its first call
      site (the RunLoop) AND `Budget.TokenBudget` gains its first
      production writer (the run-loop projection from
      `planner.token_budget`) in the same phase — the Phase-46 seam is
      un-dormanted end-to-end, not re-documented.
- [ ] `steering.RunSpec.Compression` field; the step-loop call fires only
      when `TokenBudget > 0` and the runner is non-nil; nil/zero =
      byte-identical to today (golden no-op test).
- [ ] Production wiring: `planner.token_budget` config field (validated;
      documented in `examples/harbor.yaml`); `bootDevStack` constructs
      summariser + runner when non-zero; devstack D-094 mirror.
- [ ] Long-trajectory E2E: compression fires; `trajectory.compressed`
      emitted with identity; the next prompt build takes the
      `Summary != nil` path and the prompt shrinks (byte-length assertion);
      the run completes correctly using summary-carried context.
- [ ] Summariser failure mode: a summariser error fails the compression
      LOUDLY (`trajectory.compression_failed` emitted, error propagated per
      the runner's contract) — never a silent fall-through that pretends
      compression happened.
- [ ] Godocs at `compression.go` + `planner.go:563-573` updated to the
      now-true behaviour; Wave A dormant markers removed.
- [ ] `scripts/smoke/phase-111e.sh` asserts the surface (see Smoke script
      additions).
- [ ] D-202 (reserved; logged when the phase ships) records: the summariser
      home, the single-compression scope fence, the re-compaction
      follow-up.

## Files added or changed

- `internal/llm/summarizer/trajectory.go` — **NEW** `TrajectorySummariser`
  (+ options: model override, prompt override, max summary tokens).
- `internal/llm/summarizer/trajectory_test.go` — **NEW**.
- `internal/llm/summarizer/summarizer.go` — package godoc: the
  two-interface disambiguation.
- `internal/runtime/steering/runloop.go` — `RunSpec.Compression` + the
  step-loop `MaybeCompress` call.
- `internal/planner/compression.go` + `internal/planner/planner.go` — godoc
  truth updates (no behaviour change to the runner itself).
- `cmd/harbor/cmd_dev.go` + `cmd_dev_runloop.go` — runner construction +
  `Budget`/`Compression` projection.
- `harbortest/devstack/devstack.go` — D-094 mirror.
- `internal/config/config.go` + `validate.go` — `planner.token_budget`.
- `examples/harbor.yaml` — documented field.
- `docs/recipes/configure-a-planner.md` — headless budget + compression
  section.
- `test/integration/phase111e_compression_test.go` — the long-trajectory
  E2E + failure mode.
- `scripts/smoke/phase-111e.sh` — real assertions.
- `docs/decisions.md` — D-202 (reserved; logged when the phase ships).
- `docs/plans/README.md` — status flip on ship.

## Public API surface

- `summarizer.NewTrajectorySummariser(client llm.LLMClient, opts ...TrajectoryOption) (*TrajectorySummariser, error)`
  — satisfies `planner.Summariser`.
- `steering.RunSpec.Compression *planner.CompressionRunner` — nil = off.
- Config: `planner.token_budget` (0 = off, default).
- Behavioural surface: `trajectory.compressed` /
  `trajectory.compression_failed` events now occur in production.

> Scope note: "public" here is module-internal — `internal/` packages are not
> importable by external modules (the recorded reason `harbortest/` lives at
> the top level). This surface is stable for in-module consumers (cmd,
> harbortest, examples); external-team embedding needs the future
> facade/export RFC (audit §5 / Wave D), out of scope for this phase.

## Test plan

- **Unit:** `TrajectorySummariser` prompt composition + five-field parse
  (scripted LLM responses via the mock driver); empty/garbage LLM output →
  `ErrEmptySummary` / parse error, loud; option overrides;
  budget-zero/nil-runner no-op golden in the RunLoop.
- **Integration:** `test/integration/phase111e_compression_test.go` — real
  drivers (react planner, steering RunLoop, scripted OpenAI-compatible
  `httptest` LLM server per the 83l `scriptedLLMServer` precedent — the
  summariser's Complete is a REAL wire round-trip, not a mock), the
  long-trajectory E2E from the acceptance criteria; identity propagation on
  the compression events; failure mode: summariser-errors run.
- **Conformance:** N/A — no driver seam added (the Summariser is a
  single-production-implementation interface with test fixtures; the §4.4
  registry ceremony is deliberately not minted for it — one real
  implementation, options for variation).
- **Concurrency / leak:** N≥100 concurrent runs sharing ONE
  `CompressionRunner` + ONE `TrajectorySummariser` under `-race` (both are
  compiled artifacts, D-025): no cross-run summary bleed (run A's summary
  never lands on run B's trajectory), no cancellation cross-talk (cancelling
  a mid-summarise run doesn't poison siblings), goroutine baseline restored.

## Smoke script additions

`scripts/smoke/phase-111e.sh`:

- Static: `MaybeCompress` has a non-test call site under
  `internal/runtime/steering/` (the audit's regression grep);
  `examples/harbor.yaml` documents `planner.token_budget`.
- `go test ./internal/llm/summarizer/... ./internal/runtime/steering/...
  -run 'Trajectory|Compress'` green; `go test ./test/integration/ -run
  Phase111e` green.
- Live (when classified live-server, mock-LLM escape hatch + tiny budget
  fixture): a multi-step task surfaces a `trajectory.compressed` event on
  the events stream. 404/405/501 → SKIP pre-phase.

## Coverage target

- `internal/llm/summarizer`: 90% (package's existing bar).
- `internal/planner`: 90% (godoc-only delta; existing coverage holds).
- `internal/runtime/steering`: 85%.

## Dependencies

- 46 (CompressionRunner + Summariser seam + `TrajectorySummary`), 35
  (structured output modes), 33 (bifrost driver for the live path), 107
  (streaming pipeline — compression must not disturb chunk flow; asserted
  in the E2E).
- The D-192 steering-dispatch fix (Wave A): the step loop this phase
  extends must be in its post-fix shape (per-step dispatch) so the
  compression call sits at a stable point.

## Risks / open questions

- **Staging note (Wave C):** the 111 band parallelizes freely once Wave B
  Stage 1 (110a + 110c) merges; 111e has no hard 110-band dependency (the
  run-loop wiring site moves if 110a's promotion lands first — §17.6
  covers it); all six 111-band phases are mutually independent.
- **Summary quality is model-dependent.** A weak model produces a lossy
  summary and the run degrades subtly. Mitigations: the five-field schema
  constrains the output; the E2E's "answer depends on summary-carried fact"
  assertion is the quality floor; the summariser model is overridable
  (option + config) so operators can route compaction to a stronger/cheaper
  model than the planner's.
- **Compression latency in the step loop.** `MaybeCompress` adds one LLM
  round-trip at the firing step. Accepted: it fires at most once per run
  (scope fence) and only on over-budget trajectories that would otherwise
  hit `ErrContextLeak`/window limits. The E2E records the firing step's
  latency so the cost is visible, not hidden.
- **Estimator drift.** `DefaultTokenEstimator` (`len/4+1`) mirrors the LLM
  package's estimator by design (glossary: "single surface; no parallel
  implementation per §13") — the implementor must NOT introduce a third
  estimator; any precision improvement edits both mirrored sites.
- **Ship vs. defer.** The audit offered "or mark the seam deferred in godoc
  and decisions.md". This plan recommends SHIP: the consumer half (prompt
  builder) is already live, the seam is conformant, and the value prop
  ("durable long-running agents") is hollow without it. Deferral would mean
  un-documenting a promise two RFC sections make.

## Glossary additions

- **`TrajectorySummariser`** — the production LLM-backed
  `planner.Summariser` (`internal/llm/summarizer`, Phase 111e): composes a
  compaction prompt over the serialized trajectory, calls
  `LLMClient.Complete` (structured output), parses the five-field
  `TrajectorySummary`. Distinct from the memory-subsystem `Summarizer`
  (different interface, different inputs). Add to `docs/glossary.md`.
- **`planner.token_budget`** — operator config knob (0 = off) projected
  onto `Budget.TokenBudget`; the threshold above which the RunLoop invokes
  `MaybeCompress`. Add to `docs/glossary.md` (cross-reference the existing
  **Compression budget** entry).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Cross-session isolation: compression state is per-run (no summary
      bleed across concurrent runs — asserted)
- [ ] **Primitive + consumer in the same wave (§13):** `Summariser` gets its
      production implementation, `MaybeCompress` its first call site,
      `TokenBudget` its first production writer — all exercised end-to-end
      with a test — checked.
- [ ] Concurrent-reuse test passes (runner + summariser, N≥100, `-race`)
- [ ] Integration test wires real drivers end-to-end, asserts identity
      propagation, covers ≥1 failure mode, runs under `-race`
- [ ] Config field documented in plan + example config + validated
- [ ] Glossary updated
- [ ] D-202 filed when the phase ships
