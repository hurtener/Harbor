# Phase 132 — embed-runonce

## Summary

Ships the production one-call goal runner for the embed adopter path:
`Stack.RunOnce(ctx, goal, identity, opts...)` (blocking, no `Sync`
suffix) plus the shared `runctx.NewRunContext` factory it builds the
run's `planner.RunContext` through. `NewRunContext` composes the
existing run-loop projection helpers (memory / skills / artifact /
streaming) rather than introducing a third hand-rolled construction
site, so a headless embedder runs a goal in one call instead of the
~15–27 lines of `RunContext`/`RunSpec` ceremony the prior path required.

## RFC anchor

- RFC §3.6 — the public SDK facade (`sdk/` curated alias re-exports);
  `Stack.RunOnce` gains an `sdk/assemble` alias and `NewRunContext` an
  `sdk/runctx` alias.
- RFC §6.4 — the Runtime owns tool dispatch and the run loop; RunOnce
  drives the assembled `RunLoop` with the assembled planner + executor.
- RFC §6.2 — Planner interface + `RunContext`; the factory populates the
  planner-visible `RunContext` that is the runtime's half of the planner
  contract.

## Briefs informing this phase

- `brief 01` — core runtime + streaming (the run loop, streaming surface).
- `brief 02` — planner + steering; the `RunContext` population contract.

## Brief findings incorporated

- `brief 02`: the planner never imports runtime internals — everything
  it sees arrives through `planner.RunContext`; the projection code that
  POPULATES `RunContext` is the runtime's half of the contract.
  `NewRunContext` is that population code promoted to a single shared
  factory, so a one-call runner and the dev drivers compose the SAME
  projections.
- `brief 01`: streaming is first-class and synchronous on the run
  goroutine; the factory wires the bus-backed chunk publisher
  (`RunContext.OnChunk`) so the embed path's streaming surface matches
  the dev path's (the user-facing `WithStream` sink is the sibling
  132-stream phase).

## Findings I'm departing from (if any)

None. The dev-driver bodies (`cmd/harbor/cmd_dev_runloop.go`,
`harbortest/devstack`) are deliberately NOT rewritten — their per-task
projection threads control-plane resolutions (agent-config, session
overlay, tenant overrides) the headless one-call factory omits by
design. They keep their own bodies but call the same underlying helpers,
which the parity test pins.

## Goals

- A single blocking call (`Stack.RunOnce`) turns a goal + identity into
  a terminal `planner.AnswerEnvelope`.
- A single shared `RunContext` factory (`runctx.NewRunContext`) that
  composes the established projection helpers, usable by a one-call
  runner or a headless `RunSpec` builder.
- Identity is mandatory and fails loud; a non-runnable stack fails loud
  with `ErrNotRunnable`.
- Concurrent-safe reuse of one shared `Stack` across N≥100 runs (D-025).

## Non-goals

- The user-facing `WithStream(func(StreamEvent))` sink — sibling phase
  132-stream (D-266). `RunOption` is designed extensible so it lands
  without a signature change.
- Rewriting the dev-driver per-task population bodies.
- A task-FSM-backed runner (RunOnce drives `RunLoop.Run` directly, the
  same shape the manual recipe documents); spawning through the task
  registry stays a documented variation.

## Acceptance criteria

- `runctx.NewRunContext(ctx, src, quad, goal, opts...)` returns a
  populated `planner.RunContext`; identity-incomplete input fails loud.
- `NewRunContext` composes `FetchMemoryBlocks`, `ProjectSkillsDirectory`
  (over `Directory.View`), `ResolveInputArtifacts`, and the bus chunk
  publisher — proven by a parity test asserting field-equality with each
  helper called directly.
- `Stack.RunOnce(ctx, goal, identity, opts ...RunOption)` blocks, drives
  the assembled `RunLoop`, and returns `planner.AnswerEnvelope`; a
  facade alias exists at `sdk/assemble`.
- A non-runnable stack (no planner/run loop) returns `ErrNotRunnable`.
- N≥100 concurrent `RunOnce` invocations against one shared `Stack` pass
  under `-race` with no context bleed, no cross-cancellation, and the
  goroutine baseline restored after Close.
- `docs/recipes/embed-harbor-headless.md` step 4a documents the
  shorthand; the manual 4b path is kept.
- `examples/embed-runonce/` is a real checked-in program the Phase 112b
  smoke compiles.

## Files added or changed

```text
internal/runtime/runctx/newruncontext.go          # NewRunContext + Sources + Option (new)
internal/runtime/runctx/newruncontext_test.go     # projection-parity table (new)
internal/runtime/assemble/runonce.go              # Stack.RunOnce + RunOption (new)
internal/runtime/assemble/runonce_test.go         # golden + fail-loud + N>=100 -race (new)
sdk/assemble/assemble.go                          # RunOption/WithRunID/WithInputArtifacts/ErrNotRunnable aliases
sdk/runctx/runctx.go                              # NewRunContext/Sources/Option aliases
examples/embed-runonce/main.go                    # checked-in worked example (new)
docs/recipes/embed-harbor-headless.md             # step 4a (RunOnce shorthand)
scripts/smoke/phase-112b.sh                        # leg 6 — example compile + parity/concurrency tests
docs/plans/phase-132-embed-runonce.md             # this plan
docs/plans/README.md                              # index row + detail block
docs/decisions.md                                 # D-265
docs/glossary.md                                  # RunOnce, NewRunContext
```

## Public API surface

```go
// internal/runtime/runctx (+ sdk/runctx alias)
func NewRunContext(ctx context.Context, src Sources, q identity.Quadruple, goal string, opts ...Option) (planner.RunContext, error)
type Sources struct { /* Memory, MemoryRecall, SkillsDirectory, Catalog, Artifacts, Bus, Logger, GrantedScopes, PlanningHints, LLMOverrides, Budget */ }
type Option func(*runContextConfig)
func WithInputArtifacts(ids ...string) Option
func WithInputArtifactDispositions(hints map[string]string) Option
func WithDispositionPolicy(policy planner.DispositionPolicy) Option

// internal/runtime/assemble (+ sdk/assemble alias)
func (s *Stack) RunOnce(ctx context.Context, goal string, id identity.Identity, opts ...RunOption) (planner.AnswerEnvelope, error)
type RunOption func(*runOnceConfig)
func WithRunID(runID string) RunOption
func WithInputArtifacts(ids ...string) RunOption
var ErrNotRunnable error
```

## Test plan

- **unit (parity):** `internal/runtime/runctx` — `NewRunContext`'s
  memory / skills / artifact projections field-equal the underlying
  helpers; streaming surface wired iff a bus is present; identity
  mandatory; fresh per-run `RepairCounters` / `Trajectory`.
- **unit (golden + edges):** `internal/runtime/assemble` — golden answer
  envelope, `WithRunID`, identity-mandatory fail-loud, `ErrNotRunnable`.
- **concurrency / leak:** `internal/runtime/assemble` —
  `TestRunOnce_ConcurrentReuse_NoBleedNoLeak`, N=120 against one shared
  Stack under `-race`, distinct identities, a cancelled-ctx band proving
  no cross-cancellation, goroutine baseline restored after Close (D-025
  / §11).

## Smoke script additions

`scripts/smoke/phase-112b.sh` leg 6:

- the example is sdk-facade-only and calls `RunOnce` (greps), emits no
  `internal/` import;
- `go build ./examples/embed-runonce/` is green;
- the N≥100 concurrent-reuse `-race` test and the `NewRunContext`
  parity test exist (greps) AND pass under `go test -race`.

## Coverage target

- `internal/runtime/runctx`: ≥ 80% (maintains existing).
- `internal/runtime/assemble`: ≥ 75% on the new `runonce.go`.

## Dependencies

- 110d (D-197) — `assemble.Assemble` + the `Stack` shape.
- 110c (D-196) — the production driver aggregator.

## Risks / open questions

- The factory imports `tools` + `llm` (new for `runctx`); verified no
  import cycle (neither imports `runctx`; `assemble` already imports
  both). RunOnce stays out of `steering` inside `runctx` — `RunSpec`
  wrapping lives in `assemble` (which imports `steering`).
- Streaming (`WithStream`) is intentionally deferred to 132-stream; the
  `RunOption` type is extensible so the split is a pure addition.

## Glossary additions

- `RunOnce`
- `NewRunContext`

## Pre-merge checklist

- [x] `go build ./...` + `go vet` clean on touched packages.
- [x] `go test -race` green on `runctx` + `assemble` + `sdk`.
- [x] N≥100 concurrent-reuse test under `-race`.
- [x] Recipe step 4a added; 4b kept.
- [x] `examples/embed-runonce/` compiles.
- [x] `scripts/smoke/phase-112b.sh` extended (no new smoke file).
- [x] D-265 in `docs/decisions.md`; glossary terms added.
- [x] No `Phase NN` / `D-NNN` in godoc-visible Go source.
- [x] `docs/plans/README.md` row + detail block (Pending).
