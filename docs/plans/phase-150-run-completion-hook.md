# Phase 150 — Run-completion hook: transcript egress through the tool catalog

## Summary

Ships the runtime's first run-lifecycle hook: an operator-configured **run-completion hook** fired exactly once at `RunLoop.Run`'s terminal exit — every terminal outcome (goal, no-path, constraints-conflict, cancellation, terminal error), never mid-run, never on pause — that dispatches a **faithful run transcript** (initial goal, steering-injected user messages in order, assistant steps and final answer, run metadata) to a **named catalog tool** through the existing `ToolExecutor` path. The egress path IS the tool catalog (MCP / in-proc / — once Phase 149 lands — config-declared HTTP): provenance, identity capture, policy retries, and args-free audit events all come free from the existing dispatch machinery; a bespoke HTTP egress client is the rejected alternative (D-280). A hook failure NEVER alters the run outcome — it emits `run.hook_failed` on the bus and Warn-logs; success emits `run.hook_dispatched`. Config is the yaml `runtime.hooks` block paired with a new durable, versioned `hooks` agent-config section (next-run projection per D-234), with NO new Protocol verb — the section rides the existing `agent_config.set_revision`/`get`/`diff`/`rollback` surface.

## RFC anchor

- RFC §6.17 (Run-completion hook — the new subsection this phase's RFC amendment adds; the amendment rides the same PR as this plan, so the reference resolves at merge)
- RFC §6.3 (steering and the run loop — the seam's home; the steering-payload bounds that bound transcript entries)
- RFC §6.4 (tool catalog and transports — the egress path; audit-redaction posture)
- RFC §6.13 (typed event bus — the hook's observability events)
- RFC §6.16 (Agent Registry / agent-config content — `agent_id` in the payload metadata; the versioned config surface the `hooks` section extends)

## Briefs informing this phase

- brief 02
- brief 05
- brief 06

## Brief findings incorporated

- brief 02 §5 item 2: "Steering at planner level. Draining a `SteeringInbox` *inside* the planner loop and mutating the trajectory directly would force every alternate planner to replicate it. Harbor moves the inbox into the runtime; planners observe only `RunContext.Control`." — the completion hook follows the same law: it is **runtime mechanism on the RunLoop**, not planner policy. No planner concrete knows the hook exists; a swapped planner inherits it for free. Putting transcript egress inside a planner would force every concrete to replicate it.
- brief 02 §5 item 4: "Magic strings as opcodes … Future runtime-level actions (e.g. `delegate`, `wait_event`) extend the sum, not the catalog of magic strings." — the hook adds NO new `Decision` shape and no reserved tool-name sentinel. It fires outside the decision loop entirely, after the terminal `Finish`; the dispatch reuses the existing `planner.CallTool` decision shape against the existing executor.
- brief 05 §5: "Foreground/background identity is split … **Harbor unifies under `TaskID` — runs are tasks of kind `foreground`.**" — this is WHY the hook is a RunLoop-level mechanism and not a `task.completed` bus subscriber: a plain foreground `Stack.RunOnce` completes without emitting ANY task event (verified: no generic run-completion event exists; only `task.completed`/`task.failed` from the tasks engine and the best-effort `runtime.run_cancelled` from `Engine.Cancel`). A bus subscriber would silently miss exactly the embed runs the motivation names. The RunLoop is the one seam ALL run types terminate through — `Stack.RunOnce` (`internal/runtime/assemble/runonce.go:305`) and both foreground+background task-driven runs (`cmd/harbor/cmd_dev_runloop.go:972`) all call `RunLoop.Run`.
- brief 06 §5: "Two-channel split … **unify on one bus from t=0**" and "avoid logging large payloads; prefer artifacts/resources and log references". — the hook's observability (`run.hook_dispatched` / `run.hook_failed`) rides the ONE canonical bus with SafePayload metadata only (tool name, outcome, duration, transcript byte size, entry count) — never the transcript content; and the hook itself is deliberately NOT a second generic eventing-to-external-systems channel — the tool catalog is the single egress (a "webhook subsystem" would be the §13 parallel implementation of what the catalog already does).

## Findings I'm departing from (if any)

None.

## Goals

- **One hook point, all terminals, exactly once.** `RunLoop.Run` fires the hook at its single terminal boundary via a deferred fire over the named return values, so EVERY exit path is covered: the terminal `planner.Finish` at `runloop.go:701-702` (goal), the REJECT constraints-conflict terminal, the cancel-while-paused terminal, the pause-timeout terminal, AND error returns (planner error, apply error, ctx cancellation at a step boundary). The outcome rides in the payload (`goal` / `no_path` / `cancelled` / `deadline_exceeded` / `constraints_conflict` / `error`, derived from `planner.FinishReason` + `errors.Is(err, context.Canceled)` classification). The hook NEVER fires mid-run and NEVER on pause — a pause is not an exit; `Run` does not return while a pause is outstanding.
- **The run outcome is settled first and is untouchable.** The hook runs AFTER `(fin, err)` are final; a hook error (tool not found, transport failure, timeout) can not mutate, replace, or fail the completed run. Failure posture: `run.hook_failed` on the bus + `Warn` log — never silent (§13), never escalated into the run result.
- **Egress = the tool catalog, through the existing executor.** The hook dispatches a synthetic `planner.CallTool{Tool: <configured name>, Args: <transcript payload>}` through `spec.ToolExecutor.ExecuteDecision` (`internal/runtime/steering/runloop.go:307`) — the same path planner tool calls take. What comes free, verified: identity + tool-name + transport (never args) on the `tool.invoked`/`tool.completed`/`tool.failed` audit events (`internal/tools/events.go:52-84` — the payloads carry `identity.Quadruple`, `ToolName`, `Transport`, timings only, satisfying §7 rules 6-7 by construction); the per-tool `ToolPolicy` retry/timeout shell; provenance capture. The executor holds the FULL `tools.ToolCatalog` (`internal/runtime/dispatch/dispatch.go:68`) while the planner's prompt sees the filtered `PlannerView` — so a hook target can be dispatchable without being exposed to the LLM (exclude it via tool exposure; the executor still resolves it).
- **A bounded detached context bridges the cancellation gap (§5).** For the cancelled-run outcome the run's own ctx is already dead; the hook fires under `context.WithTimeout(context.WithoutCancel(runCtx), spec.CompletionHook.Timeout)` — `WithoutCancel` preserves the ctx **values** (the identity quadruple keeps flowing; never `context.Background()`, which would drop identity) while detaching cancellation, and the explicit timeout (default 10s, configurable) keeps the detachment bounded. In-repo precedent: `internal/tools/auth/provider.go:323` and `internal/tools/auth/drivers/tokenexchange/tokenexchange.go:433` use exactly this composition for post-cancellation token work.
- **A faithful transcript, captured from live run state.** Mid-run steering USER_MESSAGE text is not durably captured anywhere today (verified: `apply.go:93-100` appends to per-step `sc.signals.UserMessages`, consumed and cleared each step; the `AppliedControl` history ring records type+time+err only, `history.go:21-34`; the sole durable record is one `Memory.AddTurn` goal/answer pair on `FinishGoal`, `cmd_dev_runloop.go:1026`). The RunLoop therefore accumulates, per run, an ordered stack-local record — `(step index, entry)` for each applied USER_MESSAGE and REDIRECT — alongside the trajectory (per-run loop state on the goroutine's stack, exactly like `carryEvents`; D-025-clean, no RunLoop-struct field). At completion the transcript assembler interleaves: the initial goal (user), steering user messages and goal redirects in arrival order (user), per-step assistant entries (`Step.AssistantPreamble` where non-empty, plus a compact `{tool, ok|err}` tool-line per invocation — never raw observations), and the final answer (assistant). Steering entries are already bounded by the Protocol-edge payload caps (≤4096 chars/string, ≤16 KiB/event — RFC §6.3), so no unbounded caller content enters the payload.
- **A typed, stable payload contract.** `steering.RunCompletionPayload` is a typed Go struct with a golden-pinned JSON encoding and `format_version: 1` — it leaves the process to operator servers, so it is treated as a public contract from day one. Metadata: the full quadruple, `agent_id` (when the wiring layer knows it; empty for bare embed runs), outcome, started/completed timestamps, duration, step count, `ToolCallsSeen` (via `planner.CountToolInvocations`, D-274 semantics). D-026 interplay, verified: the payload travels as tool ARGS to the transport — it never traverses the LLM edge, so no `ErrContextLeak` applies; the audit events around the dispatch carry no args at all (above), so no summarize/truncate retrofit is needed.
- **Config: yaml + agentcfg pairing, mirroring the shipped pattern.** Static: the reserved `RuntimeConfig` slot (`internal/config/config.go:450`, yaml key `runtime:`) gains its first body — `runtime.hooks.run_completion: {tool: <catalog name>, timeout: <duration>}`; validated in `loader.go::Validate` (unknown-empty tool name rejected; non-positive timeout defaulted); example configs updated (§10). Durable: `agentcfg.ConfigPayload` gains a `Hooks *HooksSection` sibling (`agentcfg.go:214-220`), riding the existing revision machinery — content-hash, `set_revision` (full payload, section-merge invariant preserved), server-side `diff` (a new hooks arm), `rollback`, `agent.config.revised`. Resolution at run start (next-run projection, D-234 §3): **agentcfg `hooks` section (when set) › yaml `runtime.hooks` › no hook** — a shared `projection.ActiveRunCompletionHook` helper (sibling of `ActivePromptLayers` / `ActiveLLMOverrides`, `internal/runtime/agentcfg/projection/projection.go:135/:276`) called by BOTH `cmd/harbor/cmd_dev_runloop.go` and the devstack twin (§17.6 twin discipline). An in-flight run keeps its snapshot; edits are next-run-only by construction.
- **Wire impact: types only, no new verb — lockstep still applies.** `AgentConfigPayload` is schema'd per-section on the wire (typed structs at `internal/protocol/types/agentconfig.go:110-127`, hand-mirrored to TS per D-223) — so the new `hooks` section means a new `AgentConfigHooks` wire struct + the `AgentConfigDiff` hooks arm, a hand-mirrored TS interface, `make protocol-ts-gen` manifest regen, and `make protocol-docs-gen` — all named in the acceptance criteria. No new Protocol method ships: `set_revision`/`get`/`diff`/`rollback` already carry whole payloads. (A `set_hooks` convenience verb à la `set_llm_params` is a named possible follow-up, not v1.10 scope.)
- **Identity flows; Phase 148 composes automatically.** The dispatch ctx carries the run's quadruple (preserved through `WithoutCancel`), so when the hook target is an MCP tool on a connection carrying Phase 148's per-identity OAuth binding, the per-identity bearer resolution keys off the ctx identity with zero hook-specific code — the same-wave consumer proof for 148's bearer path on a non-planner dispatch.

## Non-goals

- **Pre-run injection / a pre-run hook point.** The next-message override path already covers pre-run shaping (`runs.set_overrides` — `internal/protocol/types/runs.go:26-59` + `internal/runtime/runs/protocol/overrides.go`); an additive prepend field there is a possible future ergonomic, not this phase. The seam is deliberately designed so future hook points (pre-run, post-step) are additive decisions against D-280, not reworks.
- **Durable capture of steering USER_MESSAGE text into a store.** The hook captures from live run state at completion; durable turn-by-turn steering capture (e.g. into Memory) is a named possible follow-up with its own retention/erasure questions (session erasure, D-274 item 2 territory).
- **A generic webhook / external-eventing subsystem.** The tool catalog IS the egress. A parallel HTTP-callback path would be the §13 "two parallel implementations" smell — rejected in D-280.
- **Hook types other than run-completion.** One hook point, one consumer, this wave.
- **Hook-level retry machinery.** One dispatch attempt at the hook level; bounded retries already live in the target tool's own `ToolPolicy` shell (`TimeoutMS`/`MaxRetries`) — no second retry loop, no queues.
- **Raw tool observations or trajectory dumps in the transcript.** The transcript is conversation-shaped (goal, user messages, assistant prose, final answer, compact tool lines); consumers wanting full observations use `inspect-runs` / the Protocol trace surface.
- **A new Protocol verb.** The `hooks` section rides the existing agent-config surface (above).

## Acceptance criteria

- [ ] `steering.RunSpec` gains `CompletionHook *CompletionHookSpec` (tool name + timeout + optional agent_id metadata); `RunLoop.Run` fires it exactly once at the terminal boundary (deferred over named returns) for ALL exits — `FinishGoal`, `FinishNoPath`, `FinishConstraintsConflict` (incl. REJECT + pause-timeout), `FinishCancelled` (incl. cancel-while-paused), and error returns (mapped to `error`, or `cancelled` when `errors.Is(err, context.Canceled)`) — and never on pause or mid-run. A nil `CompletionHook` is byte-identical to today's behaviour.
- [ ] The hook dispatches through `spec.ToolExecutor.ExecuteDecision` with a synthetic `planner.CallTool` under `context.WithTimeout(context.WithoutCancel(runCtx), timeout)`; the run's identity quadruple is observable at the receiving tool. The dispatch does NOT invoke `spec.OnToolDispatched`, does NOT append a trajectory step, and does NOT alter `ToolCallsSeen` / `tool_count` (D-274 counters are planner-loop semantics; asserted by test).
- [ ] A hook dispatch error (unknown tool, executor error, timeout, nil `ToolExecutor` with a configured hook) leaves `(fin, err)` untouched, emits `run.hook_failed` (SafePayload: identity, tool, outcome, error class — no transcript, no raw error text quoting caller data) and Warn-logs; success emits `run.hook_dispatched` (identity, tool, outcome, duration, transcript bytes + entry count). Both types registered in `internal/runtime/steering/events.go` from `init()`; the events exhaustiveness test extended.
- [ ] `steering.RunCompletionPayload` (`format_version: 1`) assembles the ordered transcript — initial goal, steering USER_MESSAGE + REDIRECT entries in arrival order with step indices, assistant preambles + compact tool lines, final answer — from the trajectory + the new per-run stack-local steering accumulator; a golden test pins the JSON encoding; an ordering unit test drives interleaved user messages across steps.
- [ ] Config: `runtime.hooks.run_completion.{tool, timeout}` in `internal/config` (validated; example configs updated per §10) + the `agentcfg.ConfigPayload.Hooks` section (revision round-trip, section-merge preservation, diff hooks arm, rollback) + `projection.ActiveRunCompletionHook` with the agentcfg-over-yaml precedence pinned by test, called by both the dev binary and the devstack twin (a twin parity test per §17.6).
- [ ] Wire lockstep: `types.AgentConfigHooks` + the `AgentConfigDiff` hooks arm land in `internal/protocol/types/agentconfig.go`; the TS mirror is hand-updated; `make protocol-ts-gen` regenerates the manifest and `make protocol-ts-gen-check` passes; `make protocol-docs-gen` regenerates the Protocol reference; no new method, no version bump (additive types only).
- [ ] §13 primitive-with-consumer integration test (`test/integration/phase150_run_completion_hook_test.go`): a run with a completion hook targeting a real fixture tool (in-proc registered sink, plus an MCP-sourced leg via the `cmd/harbor-mcptest-stdio` harness where practical) receives the full ordered transcript INCLUDING a steering-injected mid-run USER_MESSAGE; identity propagation asserted AT THE RECEIVING TOOL; real drivers; `-race`.
- [ ] Failure-mode legs: (a) hook tool errors → run outcome unchanged + `run.hook_failed` observed on the bus; (b) a CANCELLED run → hook still fires with `outcome: cancelled` under the detached bounded ctx (the run ctx is cancelled before the hook fires; the test proves the dispatch survives it and respects the hook timeout).
- [ ] Concurrency: N≥10 concurrent runs with hooks against ONE shared RunLoop + executor + sink under `-race` — no cross-run transcript bleed (per-run sentinel goals/messages asserted at the sink), goroutine baseline restored. (The RunLoop is an already-shipped compiled artifact; this extends its existing D-025 suite rather than re-proving construction.)
- [ ] `scripts/smoke/phase-150.sh` flips from skeleton to real assertions (unit-test legs below).
- [ ] `docs/decisions.md` gains D-280; `docs/glossary.md` gains the new terms; `docs/plans/README.md` row + detail block land; the RFC §6.17 amendment lands in the same PR.

## Files added or changed

- `internal/runtime/steering/runloop.go` — `RunSpec.CompletionHook`, the deferred terminal fire, the per-run steering-entry accumulator
- `internal/runtime/steering/completion.go` (+ `completion_test.go`) — `CompletionHookSpec`, `RunCompletionPayload`, transcript assembly, outcome mapping, golden test
- `internal/runtime/steering/events.go` (+ `events_test.go`) — `run.hook_dispatched` / `run.hook_failed` + payloads
- `internal/config/config.go`, `internal/config/loader.go` — `RuntimeConfig.Hooks` body + validation; `examples/harbor.yaml`, `examples/dev.yaml`, `examples/serve.yaml` — the commented `runtime.hooks` block
- `internal/agentcfg/agentcfg.go` (+ tests) — `HooksSection` on `ConfigPayload`, diff arm, section-merge preservation
- `internal/runtime/agentcfg/projection/projection.go` (+ tests) — `ActiveRunCompletionHook`
- `internal/protocol/types/agentconfig.go` — `AgentConfigHooks` + diff arm; `web/console/src/lib/protocol/*` TS mirror + `wire-manifest.gen.json` regen; `docs/site/protocol/*` regen
- `cmd/harbor/cmd_dev_runloop.go`, `harbortest/devstack/devstack.go`, `internal/runtime/assemble/runonce.go` (a `WithCompletionHook` RunOption or config passthrough — decide at implementation; the RunSpec field is the one mechanism either way) — run-start resolution wiring
- `test/integration/phase150_run_completion_hook_test.go`
- `scripts/smoke/phase-150.sh`
- `docs/decisions.md` (D-280), `docs/glossary.md`, `docs/plans/README.md`, `RFC-001-Harbor.md` (§6.17 amendment)

## Public API surface

- `steering.CompletionHookSpec{Tool string; Timeout time.Duration; AgentID string}` — the per-run hook config on `RunSpec.CompletionHook`
- `steering.RunCompletionPayload` / `steering.TranscriptEntry{Role, Kind, Content, Step, At}` — the stable payload contract (`format_version: 1`)
- `steering.EventTypeRunHookDispatched` / `steering.EventTypeRunHookFailed` (+ SafePayload types)
- `projection.ActiveRunCompletionHook(ctx, reg, agentID, id) (*steering.CompletionHookSpec, bool, error)`
- `config.RuntimeConfig.Hooks` (yaml `runtime.hooks.run_completion`)
- `agentcfg.HooksSection` on `ConfigPayload`; `types.AgentConfigHooks` on the wire payload

## Test plan

- **Unit:** transcript assembly ordering golden (interleaved user messages / redirects across steps; assistant preambles; final answer; empty-trajectory edge); outcome mapping table (all five FinishReasons + error + ctx-cancelled); config validation; agentcfg revision round-trip + diff arm + merge preservation; projection precedence (agentcfg › yaml › none); event payload SafePayload posture.
- **Integration:** the §13 consumer test above — real drivers on the seam (real RunLoop, real dispatch executor, real inmem bus/state, scripted LLM), a steering USER_MESSAGE injected mid-run via the real inbox, identity asserted at the receiving tool, both failure-mode legs, `-race`. Plus the devstack/cmd twin parity leg.
- **Conformance:** N/A — no new driver seam (the hook consumes existing seams; the agentcfg section rides the existing registry conformance surface, extended with a hooks-section case).
- **Concurrency / leak:** the N≥10 shared-instance no-bleed test above; goroutine baseline after N runs with hooks (the detached hook ctx must not leak timers/goroutines — `WithTimeout` cancel deferred and asserted).

## Smoke script additions

- `scripts/smoke/phase-150.sh` (`PREFLIGHT_REQUIRES: unit-tests`): `go test -race -count=1` legs for `internal/runtime/steering` (completion + events tests), `internal/agentcfg` + `internal/runtime/agentcfg/projection` (hooks section + projection), `internal/config` (hooks validation), and `test/integration -run TestE2E_Phase150` with a `go test -list` no-match-fails guard. Skeleton parks with `skip` until the surface lands.

## Coverage target

- `internal/runtime/steering` (touched packages): ≥ 80%
- `internal/agentcfg`, `internal/runtime/agentcfg/projection` (touched lines): no regression below current package coverage
- `internal/config` (touched lines): no regression

## Dependencies

- 53 (the steering `RunLoop` — the terminal seam; D-071)
- 83i (the `steering.ToolExecutor` seam + `RunSpec.ToolExecutor` — the dispatch path; D-152)
- 92a (the agent-config registry/revision/projection primitive the `hooks` section rides; D-234)
- 132 (`Stack.RunOnce` — the embed run type the hook must cover; D-265)
- 148 (same-wave — per-identity OAuth binding on MCP connections; the hook's identity-keyed bearer path is 148's non-planner consumer)
- 149 (soft — config-declared HTTP tools as hook targets; the hook works with MCP/in-proc targets if 149 slips)

## Risks / open questions

- **Executor validity post-terminal.** The dispatch executor is a compiled artifact (D-025) — construction-immutable, safe to invoke after the run's terminal Finish; per-invocation state rides ctx/rc. The one genuine hazard is cancellation (the run ctx is dead on the cancel path), closed by the `WithoutCancel`+timeout bridge. The integration test's cancelled-run leg is the proof.
- **Approval-gated or paused hook targets.** The hook path calls `ExecuteDecision` directly — it does NOT run the runloop's mid-step `dispatchDecision` drain, so an approval-gated target has no APPROVE channel post-run and would park until the hook timeout → `run.hook_failed`. Posture: a hook target behind an approval gate is a configuration error, documented at the config surface (the hook is operator-configured, admin-scoped desired state; gating it against itself adds nothing). If reviewers want a boot/set-time rejection instead of a runtime timeout, that's a Validate-time lookup — decide at implementation, either is fail-loud.
- **Transcript size on long runs.** Steering entries are Protocol-edge-bounded and assistant prose is model-bounded, but a 100-step run's transcript is still large-ish. The payload goes only to the tool transport (no LLM edge, no bus, no logs), and MCP/HTTP transports handle MB-scale args today. If a deployment needs a cap, the tool's own policy timeout + the hook timeout bound the damage; a `max_transcript_bytes` knob is a possible follow-up, deliberately NOT speculative config now.
- **`agent_id` availability.** Task-driven runs know the agent; a bare embed `RunOnce` may not. The payload's `agent_id` is optional-empty and documented as such — it is registration metadata (D-059), never an isolation key.
- **RFC §6.17 reference resolution.** This plan cites §6.17, which exists only once the amendment fragment is applied. The plans PR must apply the amendment and this plan together or drift-audit fails — flagged for the coordinator.

## Glossary additions

- **Run-completion hook** — the runtime mechanism (Phase 150, D-280) that fires exactly once at `RunLoop.Run`'s terminal exit — every terminal outcome, never mid-run or on pause — and dispatches the run's transcript payload to an operator-named catalog tool through the existing tool-execution path. Failure never alters the run outcome (`run.hook_failed` + Warn). Configured via `runtime.hooks.run_completion` (yaml) and the versioned agent-config `hooks` section (next-run projection, D-234).
- **Run transcript (`RunCompletionPayload`)** — the typed, golden-pinned, `format_version: 1` JSON payload the run-completion hook dispatches: run metadata (quadruple, optional `agent_id`, outcome, timings, D-274 tool-invocation count) plus the ordered conversation — initial goal, steering-injected user messages and redirects, assistant steps, final answer — assembled at completion from live run state. A public contract: it leaves the process to operator servers.

(Added to `docs/glossary.md` in the same PR as this plan.)

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes (the N≥10 no-bleed test doubles as this — transcripts are session-scoped state)
- [ ] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. (The RunLoop's existing D-025 suite is EXTENDED with the hook surface — the N≥10 hook-specific no-bleed test plus the existing N≥100 loop stress with hooks enabled on a subset of runs.)
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md §17. (It consumes 53 + 83i + 92a + 132 and closes the steering→tools egress seam — the integration test is mandatory.)
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (N/A — none departed from)
