# Phase 134 — sdk-examples

## Summary

Ships the first runnable `Example_*` functions under `sdk/` — the
curated public facade had zero `_test.go` files and zero discoverable
examples, so pkg.go.dev rendered the adopter's first contact with no
worked code. This phase adds mock-LLM-backed, deterministic, offline
testable examples across the four primary adopter surfaces
(`sdk/assemble` golden `RunOnce` + the `WithStream` variant,
`sdk/planner`, `sdk/steering`, `sdk/config`). The example bodies import
only the public `sdk/` facade; the dev-only mock LLM is the single
allowed test-file blank-import exception (D-089), mirroring
`internal/runtime/assemble/runonce_test.go`.

## RFC anchor

- RFC §3.6 — the public SDK facade (`sdk/` curated alias re-exports);
  these examples are the facade's first godoc-rendered worked code, the
  adopter's first contact on pkg.go.dev.
- RFC §6.4 — the Runtime owns the run loop / tool dispatch; the
  `sdk/assemble` examples drive the assembled `Stack.RunOnce`.
- RFC §6.2 — Planner interface + `RunContext`; the `sdk/planner` example
  exercises the swappable-planner driver registry.
- RFC §6.3 — steering authn/authz; the `sdk/steering` example submits a
  scoped, validated control through the per-run inbox.

## Briefs informing this phase

- `brief 01` — core runtime + streaming (the run loop + streaming surface
  the `sdk/assemble` examples exercise).
- `brief 02` — planner + steering (the swappable Planner driver registry
  and the scoped control inbox the `sdk/planner` / `sdk/steering`
  examples exercise).
- `brief 06` — events, observability, and developer experience (the
  adopter-DevX framing: discoverable, copy-pasteable examples are the
  first-contact surface).

## Brief findings incorporated

- `brief 01`: streaming is first-class and synchronous on the run
  goroutine — the `Example_streaming` example asserts the streamed
  content tokens reassemble exactly to the terminal answer, the
  observable shape of that guarantee.
- `brief 02`: the planner never imports runtime internals; the
  swappable-planner contract is a driver registry — the `sdk/planner`
  example demonstrates `RegisteredDrivers()` after blank-importing a
  planner concrete, the seam external planner authors plug into.
- `brief 06`: developer experience is a first-class product property;
  the adopter's first contact is the godoc surface. Runnable
  `Example_*` functions render inline on pkg.go.dev and are gated by
  `go test`, so they cannot silently drift from the facade they
  document.

## Findings I'm departing from (if any)

None.

## Goals

- Every primary adopter surface of the `sdk/` facade carries at least one
  runnable, deterministic, offline `Example_*` function.
- The golden one-call path (`Assemble` → `Stack.RunOnce`) and the
  `WithStream` streaming variant are both shown.
- Example bodies import only the public `sdk/` facade (+ stdlib); the
  dev-only mock LLM is the one allowed test-file blank-import exception.
- Examples are deterministic: `// Output:` markers where stable, and they
  pass under `go test -race`.

## Non-goals

- Exhaustive examples for every `sdk/` subpackage — this phase covers the
  four primary adopter surfaces (`assemble`, `planner`, `steering`,
  `config`); the remaining facades can gain examples incrementally.
- Any production-code change. This phase adds only `_test.go` files plus
  the phase-plan / smoke / index hygiene.
- A new Protocol method, REST endpoint, CLI verb, or config field.

## Acceptance criteria

- [ ] `sdk/assemble/example_test.go` ships `Example` (Assemble →
  `Stack.RunOnce`, golden path, `// Output: goal`) and `Example_streaming`
  (`WithStream` sink reassembles the streamed content to the terminal
  answer, `// Output: true`).
- [ ] `sdk/config/example_test.go` ships `Example` (`Defaults` →
  `ValidateCore` headless profile, `// Output: inmem`).
- [ ] `sdk/planner/example_test.go` ships `Example` (`RegisteredDrivers`
  after seating the `react` driver via the `sdk/planner/react` blank
  import + `IsValidFinishReason`, `// Output: [react]` then `true`).
- [ ] `sdk/steering/example_test.go` ships `Example` (`NewRegistry` →
  `Open` inbox → `Enqueue` a scoped `CANCEL` → `Drain`, `// Output: CANCEL`).
- [ ] Every example BODY imports only `sdk/` packages (+ stdlib); the
  driver aggregator rides its `sdk/drivers/prod` facade, so the only
  `internal/` reference is the file-level dev-only mock LLM blank import
  (`internal/llm/mock`, no sdk facade by D-089) in `sdk/assemble`.
- [ ] `go test ./sdk/... -run Example` passes; `go test -race ./sdk/...`
  passes; `gofmt -l` clean; `golangci-lint run ./sdk/...` clean.
- [ ] `scripts/smoke/phase-134.sh` exists and asserts the example
  functions are present and `go test ./sdk/... -run Example` is green.

## Files added or changed

```text
sdk/assemble/example_test.go      # Example (RunOnce) + Example_streaming (WithStream) (new)
sdk/config/example_test.go        # Example (Defaults + ValidateCore) (new)
sdk/planner/example_test.go       # Example (driver registry + finish vocabulary) (new)
sdk/steering/example_test.go      # Example (control inbox round-trip) (new)
scripts/smoke/phase-134.sh        # thin static gate: examples present + go test -run Example (new)
docs/plans/phase-134-sdk-examples.md   # this plan (new)
docs/plans/README.md              # index row + detail block (Pending)
```

## Public API surface

N/A — this phase adds no exported identifiers. It exercises the existing
`sdk/` facade surface (`assemble.Assemble`, `Stack.RunOnce`,
`assemble.WithStream`, `assemble.StreamEvent`, `config.Defaults`,
`config.Config.ValidateCore`, `planner.RegisteredDrivers`,
`planner.IsValidFinishReason`, `steering.NewRegistry`, `Registry.Open`,
`Inbox.Enqueue`, `Inbox.Drain`) through runnable examples.

## Test plan

- **Unit:** the `Example_*` functions ARE the tests — `go test -run
  Example` compiles and runs each; the `// Output:` markers gate the
  deterministic ones. No separate unit tests are added.
- **Integration:** N/A — these examples do not close a cross-subsystem
  seam; they exercise the already-shipped `sdk/assemble` one-call surface
  (Deps 132 / 132-stream) through its public facade, which the
  `sdk/assemble` examples themselves prove end-to-end (Assemble through
  RunOnce with real in-memory drivers + the dev-only mock LLM).
- **Conformance:** N/A.
- **Concurrency / leak:** N/A — the examples build no new reusable
  artifact; the `Stack` concurrent-reuse guarantee is already covered by
  `TestRunOnce_ConcurrentReuse_NoBleedNoLeak` (Phase 132). The examples
  run under `-race` as part of `go test -race ./sdk/...`.

## Smoke script additions

`scripts/smoke/phase-134.sh` (new, thin + static — no live server
surface): asserts each example file exists and declares its `Example`
function(s), and that `go test ./sdk/... -run Example` is the wired gate.
Static-only, matching sibling doc/example smokes; auto-skips nothing
runtime-shaped because the phase adds no endpoint.

## Coverage target

- N/A on production coverage — this phase adds only `_test.go` files and
  touches no production package. The binding gate is `go test ./sdk/...
  -run Example` (all examples compile + run, `// Output:` matches) under
  `-race`.

## Dependencies

- 132 (D-265) — `Stack.RunOnce` + `RunOption` + `ErrNotRunnable` and the
  `sdk/assemble` aliases.
- 132-stream (D-266) — `WithStream` + `StreamEvent` + `StreamEventKind`
  and their `sdk/assemble` aliases.

## Risks / open questions

- Determinism: the mock LLM echoes the goal, so the streamed content
  equals the terminal answer exactly; `Example_streaming` asserts
  equality (`// Output: true`) rather than a brittle token count, and the
  golden `Example` prints the stable `FinishReason` (`goal`) rather than
  the echoed answer text — robust against ReAct prompt-format changes.
- The `sdk/planner` example's `RegisteredDrivers()` output is stable
  because the planner test binary blank-imports ONLY `sdk/planner/react`
  (seating exactly `react`); pulling the full `sdk/drivers/prod`
  aggregator would widen the list, so it is deliberately not imported
  there.

## Glossary additions

None — this phase introduces no new vocabulary.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target — N/A, test-only phase
- [x] If multi-isolation paths changed: cross-session isolation test passes — N/A, no identity paths changed
- [x] If this phase builds a reusable artifact: concurrent-reuse test passes — N/A, no new reusable artifact; examples run under `-race`
- [x] If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: integration test — N/A, the `sdk/assemble` examples exercise the one-call surface end-to-end with real drivers; no new seam
- [x] If new vocabulary: glossary updated — N/A, no new terms
- [x] If a brief finding was departed from: justified + decisions.md entry — N/A, no departures
