# Phase 136 — MCP agent-calls-tool integration test

## Summary

Adds a net-new end-to-end integration test proving the **agent-call leg**
for an MCP-sourced tool: a planner decides to call a tool discovered from
a real stdio MCP server, and the runtime dispatches it **through the
executor** to the live subprocess, routing the result back into the
trajectory. Closes the verification gap where MCP discovery (tools reach
the catalog) was tested but the dispatch leg was not — the only
executor-level tool-invocation test today calls an in-process builtin.

## RFC anchor

- RFC §6.4
- RFC §6.2

## Briefs informing this phase

- brief 07
- brief 03
- brief 14
- brief 15

## Brief findings incorporated

- brief 07 §1: "Harbor performs tool calling at the **runtime/orchestration
  layer**, not at the LLM provider layer … the runtime … parses the model's
  reply into a `PlannerAction`, dispatches one or more tool calls … validates
  inputs and outputs … formats observations back into the next prompt." This
  test exercises exactly that dispatch trio for an MCP tool, asserting the
  echoed observation round-trips into the planner's next prompt.
- brief 07 §"Prior-step rendering": "every prior step is rendered as …
  `{"role":"user", "content": render_observation(...)}`". The load-bearing
  assertion checks the dispatched MCP result appears in the SECOND LLM
  request's messages — the observation render, not the input args.
- brief 14 §Overview: Harbor consumes MCP servers as a client through the
  southbound driver over the official Go SDK (spec 2025-11-25); "Tool …
  *consumption* is solid; content lowering is genuinely good." The test drives
  a REAL stdio MCP server (`cmd/harbor-mcptest-stdio`) over the real wire, per
  §17.8 (fixtures derive from the real spec/server, not a hand fixture).
- brief 03 + brief 15: native tool-calling — the React planner projects a
  provider `tool_calls` block into a `planner.CallTool` decision that the
  executor resolves against the catalog by name. The test pins this for an
  MCP-registered name (`mcptest_echo`, the source-prefixed `echo` tool).

## Findings I'm departing from (if any)

None.

## Goals

- Prove a planner-emitted `CallTool` for an MCP-discovered tool dispatches
  through the executor to a real stdio MCP subprocess and the result reaches
  the trajectory + the planner's next prompt.
- Prove identity propagates end-to-end to the MCP dispatch (the MCP driver
  fails closed without the triple) and cross-identity reads are rejected.
- Cover ≥1 failure mode: bad arguments rejected server-side, surfaced through
  the executor, the planner re-plans.

## Non-goals

- No production code change — this is a test-only phase.
- No new Protocol method / REST endpoint / CLI subcommand.
- No N≥100 concurrent-reuse stress (the wave-end `wave_v18_test.go` carries
  the cross-wave concurrency stress; this test is the focused agent-call leg).

## Acceptance criteria

- [ ] `test/integration/mcp_agent_call_test.go` defines a test named EXACTLY
  `TestE2E_Phase83g_MCPAgentCallsTool`.
- [ ] The test boots the real `cmd/harbor-mcptest-stdio` server, assembles a
  devstack with it in config, and drives a goal via the run loop that invokes
  `mcptest_echo`.
- [ ] The dispatch-through-executor signal is the echoed sentinel in the
  CallTool step's OBSERVATION (not its input args) and in the second LLM
  prompt — a catalog-listing test cannot produce it.
- [ ] Identity propagation + cross-identity isolation asserted (`Tasks.Get`
  rejects a foreign tenant with `ErrNotFound`).
- [ ] A bad-args failure mode is dispatched, rejected, and re-planned.
- [ ] `go test ./test/integration -run '^TestE2E_Phase83g_MCPAgentCallsTool$'
  -race` passes and actually RUNS (not 0 tests).
- [ ] `scripts/smoke/phase-136.sh` exists and carries a `-list`/no-match-fails
  guard that fails loud on a zero match.

## Files added or changed

- `test/integration/mcp_agent_call_test.go` (new)
- `scripts/smoke/phase-136.sh` (new)
- `docs/plans/phase-136-mcp-agent-call-test.md` (this file)
- `docs/plans/README.md` (index row + detail block)

## Public API surface

N/A — test-only phase. No exported runtime surface added.

## Test plan

- **Unit:** N/A.
- **Integration:** `test/integration/mcp_agent_call_test.go` —
  `TestE2E_Phase83g_MCPAgentCallsTool` with two subtests:
  `DispatchThroughExecutor` (happy path: real bifrost LLM driver against a
  scripted OpenAI-compatible server scripts a `CallTool(mcptest_echo)` →
  `Finish`; asserts the echoed sentinel in the step observation + second
  prompt; asserts cross-identity `Tasks.Get` isolation) and
  `BadArgsRejectedThroughExecutor` (failure mode: wrong-typed arg →
  server-side rejection surfaced through the executor → re-plan). Real drivers
  on the seam (bifrost LLM driver, EventBus, StateStore, Coordinator, tools
  catalog, `mcpdrv.Provider` against a real stdio subprocess). Identity
  propagated end-to-end. Runs under `-race`.
- **Conformance:** N/A.
- **Concurrency / leak:** N/A (devstack teardown drains the subprocess; the
  wave-end test carries the concurrency stress).

## Smoke script additions

- `scripts/smoke/phase-136.sh` (classification `unit-tests`):
  - asserts `TestE2E_Phase83g_MCPAgentCallsTool` is defined in the test file
    under its exact name;
  - runs `go test -list '^TestE2E_Phase83g_MCPAgentCallsTool$' ./test/integration`
    and fails loud unless it enumerates EXACTLY that test (the no-match-fails
    guard that closes the original `…Phase83g.*Call`-matched-zero false-green).

## Coverage target

N/A — this phase adds an integration test, not a package under a coverage
gate. The test exercises the existing `internal/tools/drivers/mcp` dispatch
path + `internal/runtime/steering` run loop end-to-end.

## Dependencies

- Phase 83g (MCP southbound wired into the dev boot path; the
  `cmd/harbor-mcptest-stdio` fixture and devstack MCP-server config).
- Phase 83l (the scripted-bifrost devstack harness this test reuses).

## Risks / open questions

- The scripted LLM `tool_calls.arguments` must be CLEAN JSON (one level of
  `%q` escaping + the bifrost driver's one unescape yields valid args). Passing
  pre-escaped JSON double-escapes and the MCP wire decode fails — pinned in the
  test by asserting the happy-path dispatch SUCCEEDS (echoed sentinel present).
- The bad-args failure relies on server-side schema validation (MCP tools carry
  `Validate: nil`; the wire validates). The Go SDK rejects a number for a
  string field deterministically (json unmarshal into the typed handler args).

## Glossary additions

None — no new vocabulary introduced.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target (N/A — test-only)
- [x] If multi-isolation paths changed: cross-session isolation test passes (the test asserts cross-identity `Tasks.Get` rejection)
- [x] If this phase builds a reusable artifact: concurrent-reuse test passes — N/A, this phase adds no reusable artifact; it tests existing dispatch + run-loop surfaces.
- [x] If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists, wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, runs under `-race`.
- [x] If new vocabulary: glossary updated (N/A)
- [x] If a brief finding was departed from: justified above + decisions.md entry filed (N/A — none departed)
