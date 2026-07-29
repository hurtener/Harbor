# Phase 215 — caller-named agent selection

## Summary

`agent_id` is an explicit Protocol argument on roughly thirty request types — the whole
`agent_config.*` family, the user-layer and skills families, `tools.describe`. It is
absent from exactly one place: the surface that starts a run. `StartRequest`
(`types/control.go:94`) has no agent field, `tasks.SpawnRequest` has none, and the run
loop reads its agent from `opts.AgentConfigID`, a construction-time option seeded from
the constant `devAgentConfigID = "harbor-dev-agent"` (`serve.go:416`). The consequence
is concrete rather than hypothetical: **a caller can create agent configs that can
never run.** This phase adds the missing argument.

## RFC anchor

- RFC §6.16
- RFC §5.2
- RFC §6.2

## Briefs informing this phase

- brief 02
- brief 05

## Brief findings incorporated

- brief 02 (planner + control): the control surface is the runtime's contract with its
  callers, and a caller that cannot name what it means is forced to encode intent in a
  channel that carries no meaning. The shipped workaround — putting the agent's name in
  the human-readable `description` string — is precisely that: a label nothing reads,
  nothing validates and nothing can fail loudly on.
- brief 05 (state, tasks, sessions): a run's durable record should be able to answer
  what the run was. D-309 made the session row's agent binding representably absent
  because no populated value could be produced; a run that NAMED an agent can produce
  one, and the read side should stop being absent for that case.
- brief 02: a control surface must refuse rather than substitute. A start that names an
  agent the runtime cannot honour and silently binds a different one reproduces the
  defect this phase closes, one layer down.

## Findings I'm departing from (if any)

None. D-309's representable-absence stance is preserved unchanged for runs that name no
agent — this phase makes the field populatable, not mandatory.

## Goals

- A caller can name which agent a run executes as, on both the foreground and
  background entry points.
- Omitting the field is byte-identical to today's behaviour.
- Naming an agent that does not resolve for the caller's identity is refused at the
  Protocol edge, before a task exists.
- The named agent is persisted onto the task and surfaces on the session row.

## Non-goals

- **Any change to how a runtime binds an agent when none is named.** The existing
  resolution is untouched and remains the behaviour for every caller that omits the
  field.
- **A new entitlement plane.** Agents are already tenant-scoped registered entities
  (`AgentRegistry.Register` / `ListTenant`), so "is this agent reachable for this
  caller" is answered by the tenant-scoped registry lookup the `agent_config.*` family
  already performs. This phase validates against that; it does not invent a second
  authorization model.
- **Publication or visibility semantics** ("this agent is published to an org"). Out of
  scope by explicit decision — the consumer Console owns which agents it offers a user,
  and the runtime's job is to honour or refuse a named one.
- **Making `SessionRow.agent_id` mandatory.** D-309 stands.
- **Prompt-layer chaining across agents.** Not needed and deliberately not built — see
  the composition note below.

## The composition model this phase makes reachable (context, not new work)

The four-tier composition already ships and this phase changes none of it. Recorded here
because it is the reason the phase is small:

```text
admin Base        ConfigScopeAgent, PromptLayers.Base    ← the spine, session-unwritable
  ▸ admin User    ConfigScopeAgent, PromptLayers.User
  ▸ durable User  ConfigScopeUser                         ← per-user, persistent
  ▸ session User  sessionoverlay                          ← ephemeral
```

Two consequences settle questions that would otherwise be re-derived:

1. **Replacement versus layering is already expressible.** `PromptLayers.Base`
   overrides the configured default base; `PromptLayers.User` composes above it without
   mutating it. An admin choosing "replace" sets `Base`; choosing "extend" sets `User`.
   No `mode` discriminator is added — that would be a second way to express what field
   presence already says (§4.4).
2. **A user layering onto an admin-authored agent needs no chaining.** Both tiers key on
   the SAME `agentID` under different `ConfigScope`s, so a `ConfigScopeUser` revision
   under agent B composes below B's admin layers automatically. No parent pointers, no
   recursive resolution, no cycle detection.

The user tier's safety is structural, not validated:
`prototypes.AgentConfigUserPayload` carries no `Base` field at all, so
`userPayloadToDomain` (`user.go:37`) can only ever construct `PromptLayers{User:}`. A
user physically cannot write a base layer. This phase must not weaken that.

## Acceptance criteria

- [ ] `StartRequest` gains an optional `agent_id`; the background spawn request gains
      the same field, so the two entry points do not diverge.
- [ ] Omitted ⇒ the run binds exactly as today, asserted by a byte-comparison of the
      resolved prompt and catalog view against a pre-phase build.
- [ ] Present and resolvable ⇒ every projection resolves against the NAMED agent: LLM
      overrides, skill views, planner catalog view, prompt layers, completion hook,
      naming policy, and the three reconcile legs.
- [ ] Present and unknown, or registered under a different tenant ⇒ REFUSED at the
      Protocol edge with a typed error mapping to `CodeInvalidRequest`, before the task
      is created. Never a fallback to the default agent.
- [ ] The named agent is persisted on the task and surfaces on `SessionRow.agent_id` /
      `agent_name`; a run that named none still reports absent (D-309 unchanged).
- [ ] Two concurrent runs in one process under DIFFERENT agents do not disturb each
      other's MCP connections. The reconcile legs are already owner-scoped by
      `auth.Owner{Tenant, Agent}` (`projection.go:186`) — this is a regression assertion
      over an existing property, not new work, and it is stated because it is the
      property most likely to be broken by a careless change here.
- [ ] A user's `ConfigScopeUser` revision under a named agent composes below that
      agent's admin layers, in the documented order.
- [ ] A `ConfigScopeUser` write still cannot carry a base layer.
- [ ] Mutation-verified: reverting the edge validation turns a smoke `OK` into a `FAIL`.

## Files added or changed

- `internal/protocol/types/control.go` — the wire field.
- `internal/protocol/control.go` — edge validation + `dispatchStart` carry-through.
- `internal/tasks/tasks.go` — `SpawnRequest` field + task persistence.
- `internal/runtime/serve/runloop.go` — the run's agent id, defaulting to
  `opts.AgentConfigID` when absent.
- `internal/runtime/serve/mux.go`, `internal/runtime/serve/serve.go` — the default stays
  the boot value; the constant stops being the only reachable value.
- `internal/sessions/` — the session-row projection.
- `web/console/src/lib/protocol/` + `wire-manifest.gen.json` — TS mirror + regen
  (D-223).
- `docs/site/protocol/*.md` — regenerated (D-209).
- `test/integration/agent_selection_test.go`
- `scripts/smoke/phase-215.sh`
- `docs/decisions.md` — D-360.
- `docs/skills/` — the protocol- and tasks-surface skills (§18).

## Public API surface

```go
// internal/protocol/types
type StartRequest struct {
    // ...existing fields...

    // AgentID names which registered agent the run executes as.
    // OPTIONAL: omitted binds the runtime's configured default exactly
    // as before. Present and unresolvable for the caller's tenant is a
    // refusal, never a substitution — a caller that named A and silently
    // got B was told it succeeded, which is the defect this field closes.
    AgentID string `json:"agent_id,omitempty"`
}
```

## Test plan

- **Unit:** edge validation (empty ⇒ accepted; unknown ⇒ refused; registered under
  another tenant ⇒ refused with the same not-found shape, so existence is not
  disclosed across tenants). Carry-through from `StartRequest` to `SpawnRequest` to the
  task.
- **Integration:** `test/integration/agent_selection_test.go` — a real registry, real
  agent-config registry, real state store. Register two agents under one tenant with
  different prompt layers and different skill membership. Start a run naming each;
  assert the resolved prompt and catalog view differ and match the named agent. Start a
  run naming none; assert parity with the pre-phase default. Start a run naming a
  foreign tenant's agent; assert refusal and that no task row was written. Assert a
  `ConfigScopeUser` layer composes under the named agent. Concurrency stress: N≥10
  interleaved runs alternating between the two agents, asserting no connection thrash
  and no cross-agent prompt bleed.
- **Conformance:** N/A — no persistence interface gains a method.
- **Concurrency / leak:** the control surface and the run loop are compiled artifacts
  under D-025. N≥128 concurrent starts across two agents and two tenants under `-race`,
  asserting each run resolves its own agent's config and goroutines return to baseline.

## Smoke script additions

- `start` with no `agent_id` succeeds and behaves as before.
- `start` with a known `agent_id` succeeds and the task/session read-back reports it.
- `start` with an unknown `agent_id` returns 400 and no task is created (asserted by a
  `tasks.list` count before and after).
- `agent_config.set_revision` under a second agent id, then `start` naming it, then
  assert the run's resolved prompt reflects that agent's layer.
- Skip-if-404 across the block.

## Coverage target

- `internal/protocol`: 85%
- `internal/tasks`: 85%
- `internal/runtime/serve`: 80%

## Dependencies

- 206 (the owner-scoped registry whose `(tenant, agent)` partitioning makes concurrent
  multi-agent reconcile safe)
- 211 (the sibling owner-scoped mutators)

## Risks / open questions

- **The Console selector needs a way to list a runtime's agents.** `AgentRegistry`
  exposes `ListTenant` internally; whether a Protocol method surfaces it has NOT been
  verified in this plan. §13 forbids a Console page shipping without its feeding
  Protocol surface, so the phase author must confirm this early: if no listing method
  exists, it lands in this phase or the phase immediately before the Console work, not
  after.
- **Sizing was revised upward once and then back down; the record matters.** An earlier
  reading held that the projections were per-runtime and would need restructuring. They
  are not — every one takes `agentID` as an ordinary argument
  (`ActiveLLMOverrides(ctx, reg, agentID, id)` and siblings), so the change at that
  layer is a parameter swap. The genuinely stateful reconcile legs were already
  partitioned by `auth.Owner{Tenant, Agent}`. The work is concentrated at the edge
  (validation) and the boundary (carry-through), not in the projections.
- **Refusing on "unresolvable for the caller" needs the refusal to be indistinguishable
  from "does not exist"**, or the error becomes a cross-tenant existence oracle. Called
  out as an acceptance criterion for that reason.
- **A default agent that is not registered.** `devAgentConfigID` is a bare constant, not
  a registered entity. If validation requires registry membership, the default path must
  either bypass validation (it is not caller-named) or the dev agent must be registered
  at boot. The phase author picks one and records it; silently exempting the default
  would leave the validation asserting less than it appears to.

## Glossary additions

- **Caller-named agent** — an agent explicitly identified on a run-start request, as
  opposed to the runtime's configured default. A caller-named agent is validated and
  refused loudly; a defaulted one is resolved as before.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] `make protocol-ts-gen-check` and `make protocol-docs-gen-check` pass
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Cross-session AND cross-tenant isolation tests pass
- [ ] Concurrent-reuse test passes — N≥128 concurrent starts under `-race`
- [ ] Integration test wires real drivers, asserts identity propagation, covers ≥2
      failure modes, runs under `-race`
- [ ] If new vocabulary: glossary updated
- [ ] Operator skills updated (§18)
