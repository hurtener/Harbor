# Phase 83l — real-bifrost-tests

## Summary

Phase 83l closes the audit-lesson gap D-151 named explicitly: every
existing dev-binary integration test uses the mock LLM driver, which
does NOT exercise the bifrost-side structural validation, does NOT
fire on a missing `Model` field, does NOT push reasoning details, and
does NOT round-trip prompts that include trajectory observations. Two
real-bifrost+real-stack bugs (83h V2 + 83i missing-trajectory) sat
untested through Wave 13/14/15 audits because the mock masked them.

This phase adds a tight integration test suite at
`test/integration/phase83l_real_bifrost_test.go` that exercises the
production LLM path end-to-end: the bifrost driver, the safety/correction/
retry wrapper chain, the planner, the steering RunLoop, the
ToolExecutor seam, the trajectory append, and the memory writeback —
all against a scripted OpenAI-compatible fake server backed by
`httptest.NewServer`. The fake server records every request the dev
stack made so tests can assert wire-level invariants the mock could
never reach.

**Bonus production-bug fix surfaced + closed in the same PR (CLAUDE.md
§17.6):** the moment the first real-bifrost integration test ran, it
exposed a latent production bug — `cmd/harbor/cmd_dev.go::bootDevStack`
constructed the `llm.ConfigSnapshot` WITHOUT including
`cfg.LLM.CustomProviders`, `cfg.LLM.NetworkDefaults`, or
`cfg.LLM.Corrections`. An operator who declared a custom-provider
(NIM / vLLM / ollama / in-house gateway) in `harbor.yaml` would pass
config validation but fail at boot with `bifrost: invalid provider
… declared custom: (none)`. The fix adds three new projection
helpers (`copyCustomProviders`, `copyNetworkDefaults`,
`disableCorrectionsFromConfig`) wired into the snapshot at both
`cmd/harbor/cmd_dev.go::bootDevStack` AND
`harbortest/devstack/devstack.go::tryAssemble` (D-094 mirror). This is
exactly the failure mode D-151's audit lesson predicted: the mock LLM
masked the path, every existing test passed, real bifrost would have
broken at first contact.

## RFC anchor

- RFC §6.5 — LLM client + provider correction (the path 83l exercises).
- RFC §6.2 — Planner subsystem (the consumer wired against the path).

## Briefs informing this phase

- brief 08
- brief 03

## Brief findings incorporated

- brief 08 §1 — bifrost speaks OpenAI-compatible chat completions
  for every provider. A scripted `httptest.NewServer` that mimics
  `/v1/chat/completions` is sufficient to exercise the entire bifrost
  driver path; no external subprocess is needed.
- brief 03 §3 — A tool registered through `inproc.RegisterFunc` is
  the same shape as a built-in or a custom tool. The test wires
  `text.echo` (V1.1 built-in from 83n) so the assertion surface
  matches the shipped operator path.

## Findings I'm departing from (if any)

None.

## Goals

- One new integration test file at
  `test/integration/phase83l_real_bifrost_test.go`. Two tests:
  - **`TestE2E_RealBifrost_PlannerExecutorTrajectory_HappyPath`** —
    the canonical end-to-end shape. Scripts the fake LLM to return
    one CallTool (text.echo) followed by one Finish; asserts the
    full sequence of bus events fires, the second LLM request's
    prompt carries the first request's observation (trajectory
    append worked), every LLM request carries `Model` (83h V2 fix),
    and `text.echo` was invoked exactly once.
  - **`TestE2E_RealBifrost_ToolFailure_PlannerReplans`** — the
    failure-mode shape. Scripts the fake LLM to call a built-in
    tool with bad args (validator rejects); the runtime returns the
    error as the step's observation; the planner re-plans with the
    error context and returns Finish with an apology.
- A small fake-LLM helper (`scriptedLLMServer`) at the top of the
  same file. Records every request, replays scripted responses in
  order, fails loud (HTTP 500) when the script is exhausted.

## Non-goals

- **Streaming SSE coverage.** The bifrost driver's unary path is
  what the dev binary uses; streaming (`OnReasoning` hook) is a
  separate test surface. V1.1 closes the unary regression hole;
  streaming gets its own coverage when a phase touches the streaming
  surface.
- **Multi-step CallParallel / SpawnTask / AwaitTask.** The dev
  binary's `devToolExecutor` returns `ErrDecisionShapeUnsupported`
  for these (Phase 83i / D-152); they have their own dedicated
  coverage where the executor implements them.
- **Provider-correction layer behaviour.** Phase 34's per-provider
  rewrites have dedicated tests. The 83l tests set
  `corrections.enabled: false` so the wire shape is asserted against
  what the planner asked for, not against what corrections rewrote.
- **A full wave-end E2E.** That's the §17.5 audit's job at the wave
  boundary; 83l is one targeted hole-plug.

## Acceptance criteria

- [ ] `test/integration/phase83l_real_bifrost_test.go` lands with
      the two tests + the `scriptedLLMServer` helper.
- [ ] The happy-path test asserts: ≥2 LLM requests reached the fake
      server, every request carried a non-empty `model`, the second
      request's `messages` includes the trajectory observation from
      the first step, the bus emitted `planner.decision` +
      `planner.finish` + `tool.invoked` + `tool.completed` +
      `llm.cost.recorded`, and `text.echo` ran exactly once.
- [ ] The failure-path test asserts: ≥3 LLM requests (initial call →
      error observation → re-plan → finish), the bus emitted
      `tool.failed`, the final Finish answer references the failure.
- [ ] Both tests pass under `go test -race`.
- [ ] `scripts/smoke/phase-83l.sh` static-asserts the two test
      functions are present + the scripted-server helper is in
      place.

## Files added or changed

- `test/integration/phase83l_real_bifrost_test.go` — NEW; the
  scripted-server helper + the two end-to-end tests.
- `cmd/harbor/cmd_dev.go` — production-bug fix: thread
  `CustomProviders` + `NetworkDefaults` + `Corrections` into the
  `llm.ConfigSnapshot` via three new projection helpers
  (`copyCustomProviders`, `copyNetworkDefaults`,
  `disableCorrectionsFromConfig`).
- `harbortest/devstack/devstack.go` — D-094 mirror of the production
  fix.
- `docs/plans/README.md` — Phase 83l row + flip to Shipped.
- `docs/decisions.md` — D-155.
- `docs/glossary.md` — `scriptedLLMServer` entry.
- `docs/plans/phase-83l-real-bifrost-tests.md` — this plan.
- `scripts/smoke/phase-83l.sh` — static-surface assertions.

## Public API surface

None — Phase 83l is purely test coverage. The helpers are private
to the integration test file.

## Test plan

- **Unit:** N/A (the phase IS the test).
- **Integration:** the two tests above.
- **Conformance:** N/A.
- **Concurrency / leak:** the two tests run under `-race`. The
  scripted server is goroutine-local to `httptest.NewServer` (its
  lifecycle is the test's lifecycle).

## Smoke script additions

`scripts/smoke/phase-83l.sh` asserts:

- The integration-test file exists at the documented path.
- The two test function names are present.
- The scripted-server helper is present.

## Coverage target

- `test/integration`: 80% (existing).

## Dependencies

- Phase 33 (bifrost driver).
- Phase 33a (custom-provider wiring — the test uses an
  operator-style custom provider for the fake endpoint).
- Phase 45 (ReAct planner — the consumer being exercised).
- Phase 83h (default `req.Model` fill — one of the regressions
  asserted against).
- Phase 83i (RunContext wiring closure — the trajectory append
  assertion).

## Risks / open questions

- **Test fragility against bifrost network defaults.** The fake
  server runs in-process; per-request timeout overrides keep the
  tests deterministic. `Timeout: 10s` on the custom_provider gives
  ample headroom while still failing loud on a genuine hang.
- **Concurrent test flakiness.** Both tests use a per-test
  `httptest.NewServer` + a per-test devstack — no shared state.
  `-race` should be clean.

## Glossary additions

- **scriptedLLMServer** — the test-only fake HTTP server that mimics
  an OpenAI-compatible `/v1/chat/completions` endpoint. Records
  every request the dev stack made (for wire-level assertions) and
  replays a scripted JSON-response sequence keyed by request index.
  Lives in `test/integration/phase83l_real_bifrost_test.go`.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Concurrent-reuse — N/A: the phase ships test code only; the
      production paths exercised by the tests already carry D-025
      coverage
- [ ] Integration test exists per §17 — the phase IS two integration
      tests against real drivers
- [ ] Glossary updated
- [ ] If a brief finding was departed from: N/A
