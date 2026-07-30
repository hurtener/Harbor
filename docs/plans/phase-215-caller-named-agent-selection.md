# Phase 215 — caller-named agent selection

## Summary

`agent_id` is an explicit Protocol argument across the `agent_config.*` family, the
user-layer and skills families, and `tools.describe`. It is absent from exactly one
place: the surface that STARTS a run. `StartRequest` (`internal/protocol/types/control.go:94`)
has no agent field, `tasks.SpawnRequest` (`internal/tasks/tasks.go:224`) has none, and the
run-loop driver reads its agent from a CONSTRUCTION-TIME option — `d.agentConfigID`,
seeded once from `opts.AgentConfigID` (`internal/runtime/serve/runloop.go:329,:426`) whose
only production value is the constant `devAgentConfigID = "harbor-dev-agent"`
(`internal/runtime/serve/serve.go:416` → `:571`, `:637`). The consequence is concrete: a
caller can write an agent config under a new `agentID`, read it back, diff it, roll it
back — and no run will ever use it.

This phase adds the missing argument, and answers the two questions the first draft got
wrong. Both are settled here rather than left to the implementor:

1. **The credential acting principal stays BOOT-DERIVED.** A caller-named agent changes
   config projections. It does NOT change credential identity. §"Ruling A".
2. **Validation accepts an agent if EITHER its id equals the runtime's configured
   default OR a config revision exists for `(tenant, agentID)`.** There is no
   registry-membership check to reuse — the one the first draft named does not exist —
   and no cheap correct substitute. §"Ruling B".

Every factual claim below carries a `file:line`. The first draft's central defect was an
asserted validation mechanism that is not in the tree.

## RFC anchor

- RFC §5.2 — what the Protocol exposes: the task control surface `start` sits on.
- RFC §5.5 — authentication: identity is mandatory at the edge; authority is read from
  the verified context, never inferred from a body field.
- RFC §6.2 — the planner runtime the projections feed.
- RFC §6.16 — Agent Registry: `agent_id` as a registration identity, explicitly NOT an
  isolation principal.

## Briefs informing this phase

- brief 02
- brief 05

## Brief findings incorporated

- **brief 02, "Pause-state serialisation (the contract that MUST FAIL LOUDLY)"
  (`docs/research/02-planner-and-control.md:302-322`)**: the named anti-pattern is
  "try to round-trip, return null on failure" — a control path that SUBSTITUTES a
  degraded value where it cannot honour the requested one, without telling anyone. Read
  forward to this surface: a `start` that names agent A, cannot honour it, and silently
  binds the default B is the same shape with a different payload — the caller is told it
  succeeded. Hence the refusal is at the edge, before a task exists, and is never a
  fallback.
- **brief 05, `Task` struct (`docs/research/05-state-tasks-artifacts-sessions.md:149-169`)**:
  the durable per-run record the brief sketches carries `SessionID` / `TenantID` /
  `UserID` / `Kind` / `Query` and NO agent field — the shipped `tasks.Task`
  (`internal/tasks/tasks.go:151-210`) matches. The task is the record with exactly one
  agent for its whole life, so it is where a named agent belongs. The session is not
  (see "Findings I'm departing from").
- **brief 05:314 (concurrency tests)**: "N concurrent sessions × M concurrent tasks each,
  asserting no cross-talk." The D-025 run here is N≥128 concurrent starts across two
  agents and two tenants against ONE shared `ControlSurface` and ONE shared
  `RunLoopDriver`, because a per-run agent id held on the driver instead of read per run
  would bleed in exactly that shape.
- **brief 05:317 (cross-tenant isolation)**: "Storing under tenant A and attempting to
  read under tenant B fails." The validation read inherits this structurally rather than
  by a check — see Ruling B's non-disclosure property.

## Findings I'm departing from (if any)

Two. Neither is "None".

**1. brief 05's "a run's durable record should answer what the run was", as the first
draft applied it to the SESSION row.** The draft carried an acceptance criterion that the
named agent "surfaces on `SessionRow.agent_id` / `agent_name`". That reintroduces exactly
the single-valued session→agent binding **D-309 refused**, and the producer pins the
refusal in code: `internal/sessions/protocol/lister_projector.go:199-205` states that the
agent fields "stay NIL: no single-valued session→agent binding exists in V1 (a session
may run several agents), so their absence is REPRESENTABLE", and a
`projector-field-set pin test` holds `projectRow` to exactly the lifecycle field set
(§17.8). D-309's own text (`docs/decisions.md:8083`) gives the reason: "a session may run
multiple agents over its life, so there is no authoritative single agent to name."

**Ruling: persist the named agent on the TASK ONLY.** A task has one agent for its entire
lifetime; a session does not. `SessionRow.AgentID` / `AgentName` stay nil, the
`filter.agent_ids` loud-reject stays loud, and the pin test stays green. D-309's named
follow-up ("a first-class 'last agent bound to this session' read",
`docs/decisions.md:8088`) is untouched and still open — this phase makes it *cheaper* to
build later (the task rows now carry the data a per-session projection would aggregate)
without pre-deciding its shape. The departure is from the brief's *read*, not from
D-309; D-309 is upheld.

**2. The master-plan detail block for this phase asserts a validation mechanism that does
not exist.** `docs/plans/README.md:358` states validation "reuses the tenant-scoped
registry lookup the `agent_config.*` family already performs", and cites
`projection.go:186` for the owner tag. Both are wrong: `agent_config.*` performs no
registry lookup at all (Ruling B), and the owner tag is at `projection.go:184`
(`:259`, `:334` for the sibling legs). Per §2 the phase plan outranks the master plan, and
per §4.2 item 11 a permanent deviation is reflected in the master plan's detail block in
the SAME PR. Both corrections land here.

## Ruling A — the credential acting principal stays boot-derived

**Verified chain.** `internal/runtime/serve/runloop.go:1285`:

```go
runCtx := tools.WithInvokingAgent(d.subCtx, d.agentConfigID)
```

`tools.WithInvokingAgent` (`internal/tools/agent_provenance.go:33`) stamps a ctx value;
`tools.InvokingAgentFrom` (`:45`) reads it. It has two consumers:

- `internal/tools/drivers/mcp/mcp.go:1400` — southbound `_meta.agent_id` attribution.
- `internal/tools/auth/drivers/tokenexchange/tokenexchange.go:857-859` —
  `form.Set("actor_token", agentID)` + `form.Set("actor_token_type", …)`, the RFC 8693
  actor token, gated on the operator's `include_actor_token` opt-in.

`internal/config/config.go:1330-1331` states verbatim: *"The actor token is the runtime's
VERIFIED acting principal — never a client-supplied field."* `:1334-1341` documents that
the exchanged token is cached at `(scope, tenant, user, source)` granularity with the
acting principal **deliberately not in the key** — so within one cached token's TTL a
second acting principal under the same `(tenant, user)` reuses a token minted under the
first.

**Ruling: `runloop.go:1285` continues to read `d.agentConfigID` — the BOOT value —
unchanged.** A caller-named agent changes prompts, tools, skills, LLM overrides,
completion hooks, naming policy and the reconcile legs. It changes NOTHING on the
credential plane.

Two independent reasons, either sufficient:

1. **Threading the caller's value through would make a client-supplied string the RFC 8693
   actor token**, contradicting the config godoc's own invariant by name and handing a
   caller the ability to assert an acting principal to an external authorization server.
   That is a §7 credential-plane change, not a control-surface convenience.
2. **The cache would not honour it anyway.** Because the acting principal is not in the
   cache key, a run naming agent B under an already-cached `(tenant, user)` would present
   a token minted under agent A's assertion — the exchange would not even re-run. A
   "fix" that threads the value through therefore produces an actor token that is
   *sometimes* the named agent and *sometimes* a stale different one: silently
   nondeterministic credential identity, which is the §13 silent-degradation shape on the
   most sensitive plane Harbor has.

**Consequence for the implementor: there are now TWO agent-id carriers on a run, with
different provenance, and they must not be conflated.**

| carrier | value | consumers | may a caller influence it? |
|---|---|---|---|
| `d.agentConfigID` → `tools.WithInvokingAgent` (`runloop.go:1285`) | boot-derived | MCP `_meta.agent_id`, RFC 8693 `actor_token` | **No** |
| the run's effective config agent id (this phase) | caller-named, else boot | the eleven `projection.*` reads | Yes, after validation |

This is stated as an acceptance criterion AND recorded in D-360 so a later contributor
does not "unify" the two as a tidying refactor. If a future phase genuinely wants a
caller-influenced actor token, the prerequisites are (i) the cache key gains the acting
principal and (ii) an RFC PR on §7 — both named here so they are not re-derived.

## Ruling B — the two-check validation rule

**The seam the first draft named does not exist.** `agent_config.*` performs NO
agent-registry lookup. `internal/runtime/agentcfg/protocol/service.go:894-904`
(`identityFromScope`, called at `:734`, `:756`, `:834`, `:857`, `:876`) validates the
identity triple and then checks only `if agentID == ""`. And the Service's `registry`
field is `agentcfg.Registry` (`service.go:159`) — the **config** registry, not
`registry.AgentRegistry`. That absence is precisely why orphan configs exist; it is the
bug, not a mechanism to inherit.

**There is no cheap correct replacement in the agent registry.**

- `registry.Registry.Get` (`internal/runtime/registry/registry_impl.go:278`) resolves
  through `loadRecord` (`:633`) at the FULL quadruple — `state.Load` keyed by
  `identity.Quadruple{Identity: ident}` (`:638`). An agent registered in another session
  of the same tenant is not found. It would refuse nearly everything.
- `List` (`:295`) is triple-scoped by the same `requireIdentity` (`:580`).
- `ListTenant` (`:335-372`) is the only tenant-scoped read. Its own godoc names it "the
  admin-widened fleet read"; it calls `store.ListKind(ctx, state.ListScope{MaintenanceScoped: true}, …)`
  — a whole-store scan — and then filters `if ar.Identity.TenantID != tenantID { continue }`
  **in Go** (`:365-367`). That is the "fetch all then filter in Go" shape §6 rule 2 names,
  behind a maintenance-scope claim, on the hot path of every `start`. Not acceptable.
- **And it would reject the one id that works today.** The boot agent is never registered
  as a fleet entity — `internal/runtime/serve/mux.go:378-381` says so verbatim ("the boot-configured
  agent every process serves through but never registers as a fleet entity"), which is why
  `mux.go:384-390` wires `WithDefaultAgent` so the projector can SYNTHESISE a row
  (`registry_projector.go:225-247`, `IsDefault: true` at `:246`). Registry-membership
  validation would refuse `harbor-dev-agent`.

**There is no base revision either.** `agentcfg.Registry.Active`'s contract
(`internal/agentcfg/agentcfg.go:555-557`) is: *"No active pointer returns (zero, false, nil)."*
An agent nobody has configured — which is exactly the default agent's state today —
answers `ok == false`. So "a revision exists" alone would refuse the default path.

**The rule.** A `start` naming `agentID` is accepted iff EITHER:

- **(i)** `agentID` equals the runtime's configured default agent id (`opts.AgentConfigID`, the
  same value `mux.go:384` wires into `WithDefaultAgent`), OR
- **(ii)** `Active(ctx, Quadruple{Identity{TenantID: caller.TenantID, …}}, agentID, ConfigScopeAgent)`
  returns `ok == true`.

Otherwise the request is refused with `CodeInvalidRequest`, before `tasks.Spawn` runs.

**Why check (ii) is the right read and costs nothing new.** It is BYTE-IDENTICAL to the
read every run-start projection already performs: `reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID, agentcfg.ConfigScopeAgent)`
appears at `internal/runtime/agentcfg/projection/projection.go:171, :249, :324, :394,
:488, :574, :642, :706, :834, :885`. "A config revision exists for `(tenant, agentID)`"
is therefore exactly "this agent has something to project" — the edge asks the same
question the run is about to ask, one RPC earlier. No new query shape, no new store
surface, no new index.

**Why it is tenant-scoped and cannot become a cross-tenant existence oracle — structurally,
not by a check.** `ConfigScopeAgent` keys through
`syntheticQuad(id.TenantID, agentID)` (`internal/agentcfg/drivers/statestore/statestore.go:93-97`
→ `:180-186`), which puts the CALLER's tenant in the tenant slot and the agent id in the
session slot. A foreign tenant's agent is simply not present under the caller's tenant, so
"registered in another tenant" and "never existed" produce the identical `ok == false`
with no branch to get wrong. The first draft carried this as an acceptance criterion over a
mechanism that could have leaked; here it is a property of the key layout. The test asserts
it anyway (§17.6 — an inert guard is still worth pinning, and a future key-layout change
must fail this test).

**The unwired seam fails CLOSED.** `SessionEnsurer` (`internal/protocol/protocol.go:123`,
`:127-133`) is the precedent shape — a narrow consumer-side interface, optional, whose
absence SKIPS the behaviour. Copying that posture here would be a bug: a surface built
without the resolver would accept ANY `agent_id` and hand it to a driver that ignores it.
So the resolver is optional in the CONSTRUCTOR (a control-only Runtime still builds) but
**a non-empty `agent_id` against a surface with no resolver is refused**, never accepted
and never silently ignored (§13). An omitted `agent_id` on an unwired surface is
byte-identical to today.

## Goals

- A caller can name which agent a run executes as, on both the foreground `start` and the
  background spawn path, so the two entry points do not diverge.
- Omitting the field is byte-identical to today's behaviour on every plane.
- A named agent that satisfies neither check is REFUSED at the Protocol edge, before a
  task exists — never substituted.
- The named agent is persisted on the task and drives every one of the eleven
  `projection.*` reads for that run.
- The credential plane is provably untouched (Ruling A), pinned by a test.

## Non-goals

- **Any change to how a run binds an agent when none is named.** Unchanged for every
  caller that omits the field.
- **Any change to the credential plane.** Ruling A. No change to `tools.WithInvokingAgent`,
  its call site, the token-exchange actor token, or the token cache key.
- **`SessionRow.agent_id` / `agent_name`.** D-309 stands; see the departure section. The
  session projector is not touched and its field-set pin test is not relaxed.
- **A new entitlement or publication plane.** "Which agents may this user run" and "which
  agents are published to an org" are not answered. The runtime's job is to honour a
  resolvable named agent or refuse it; a consumer decides what to offer.
- **An agent-registry membership requirement.** Ruling B — deliberately NOT the rule.
- **Prompt-layer chaining across agents.** Not needed; see the composition note.
- **A new Protocol method.** `agents.list` already ships (see Risks). No
  `ProtocolVersion` bump — the field is additive.
- **A new error code.** The refusal reuses `CodeInvalidRequest`
  (`internal/protocol/errors/errors.go:49`).
- **A `bodyscope` change.** `StartRequest` is already registered
  (`internal/protocol/bodyscope/coverage.go:82` → `SurfaceControlTask`); adding a field to
  a registered type does not move its row. Verified so no one adds a redundant one.

## The composition model this phase makes reachable (context, not new work)

The four-tier composition already ships (`projection.go:938-962`, `composeUserLayer:966`)
and this phase changes none of it. Recorded because it is the reason the phase is small:

```text
admin Base        ConfigScopeAgent, PromptLayers.Base    ← the spine, session-unwritable
  ▸ admin User    ConfigScopeAgent, PromptLayers.User
  ▸ durable User  ConfigScopeUser                         ← per-user, persistent
  ▸ session User  sessionoverlay                          ← ephemeral
```

Two consequences that would otherwise be re-derived:

1. **Replacement versus layering is already expressible.** `PromptLayers.Base` overrides
   the configured default base; `PromptLayers.User` composes above it. An admin choosing
   "replace" sets `Base`, choosing "extend" sets `User`. No `mode` discriminator is added —
   that would be a second way to express what field presence already says (§4.4).
2. **A user layering onto an admin-authored agent needs no chaining.** Both tiers key on
   the SAME `agentID` under different `ConfigScope`s —
   `syntheticQuad(tenant, agentID)` vs `userQuad(tenant, realUser, agentID)`
   (`statestore.go:180-199`) — so a `ConfigScopeUser` revision under agent B composes below
   B's admin layers automatically. No parent pointers, no cycle detection.

The user tier's safety is structural, not validated: `AgentConfigUserPayload` carries no
`Base` field, so `userPayloadToDomain` (`internal/runtime/agentcfg/protocol/user.go:37`)
can only construct `PromptLayers{User:}`. This phase must not weaken that; an acceptance
criterion pins it.

## Acceptance criteria

- [ ] `StartRequest` gains an optional `agent_id` (`internal/protocol/types/control.go`);
      `tasks.SpawnRequest` and `tasks.Task` gain the matching field, so the foreground and
      background entry points do not diverge and the value is durable.
- [ ] **Omitted ⇒ byte-identical to today.** Asserted by comparing the resolved prompt
      bundle, the resolved LLM overrides and the planner catalog view of a
      no-`agent_id` run against a run on a build without the field.
- [ ] **Present and accepted under check (i) or check (ii)** ⇒ all ELEVEN
      `projection.*` reads (`projection.go:171, :249, :324, :394, :488, :574, :642, :706,
      :834, :885` — LLM overrides, skill views, planner catalog view, prompt layers,
      completion hook, naming policy, loading mode, and the three reconcile legs) resolve
      against the NAMED agent.
- [ ] **Present and satisfying neither check** ⇒ refused with `CodeInvalidRequest` at
      `dispatchStart`, BEFORE `s.tasks.Spawn` — the same reject-early posture the
      `output_schema` edge validation already holds (`internal/protocol/control.go:341-357`,
      "no task is created that the edge could have rejected"). Never a fallback to the
      default agent.
- [ ] **A foreign tenant's agent id and a never-existing agent id produce the IDENTICAL
      refusal**, so the edge is not a cross-tenant existence oracle. Structural via
      `syntheticQuad` (Ruling B); pinned by test regardless.
- [ ] **A non-empty `agent_id` against a surface built with no agent resolver is
      REFUSED**, not accepted and not ignored (§13 fail-closed). An omitted `agent_id`
      against the same surface is byte-identical to today.
- [ ] **The runtime's configured default id is accepted without a config revision.**
      A `start` naming `harbor-dev-agent` on a runtime that has never had a revision
      written for it succeeds — check (i). Mutation-verified: deleting check (i) must turn
      this into a FAIL.
- [ ] **CREDENTIAL PLANE UNCHANGED (Ruling A).** `runloop.go:1285` still reads
      `d.agentConfigID`. A run naming agent B under a runtime booted as agent A stamps
      `_meta.agent_id = A` southbound and, with `include_actor_token: true`, sends
      `actor_token = A`. Asserted directly, not by inspection: a test drives a
      caller-named run through the MCP `_meta` builder
      (`internal/tools/drivers/mcp/mcp.go:1400`) and through the token-exchange form
      builder (`tokenexchange.go:857`) and asserts the BOOT value on both.
- [ ] **The run's effective config agent id is per-run state, never a field on
      `RunLoopDriver`.** It is a local in `runOne` threaded as a parameter (the shape the
      projections already take), or an equivalent per-run ctx value — D-025. A mutable
      field on the driver is a rejection.
- [ ] **`runOne` reads the task BEFORE the reconcile legs.** Today
      `d.reconcileConnections(taskCtx, q)` runs at `runloop.go:840` and `d.tasks.Get` at
      `:849`. Since the reconcile legs must run under the run's effective agent, the Get
      moves above the reconcile. Called out because it is an ordering change in an
      already-shipped function and is the single most likely place to introduce a
      regression here.
- [ ] **Two concurrent runs in one process under DIFFERENT agents do not disturb each
      other's MCP connections.** A regression assertion over an existing property, not
      new work: the three reconcile legs are already owner-scoped by
      `auth.Owner{Tenant: id.TenantID, Agent: agentID}` (`projection.go:184`, `:259`,
      `:334`; the owner tag is ENFORCED at the registry — a cross-owner `Deregister`
      answers not-found, `internal/tools/drivers/mcp/registry.go:550-552`). Stated because
      it is the property most likely to be broken by a careless change here.
- [ ] A user's `ConfigScopeUser` revision under a named agent composes below that agent's
      admin layers, in the documented order.
- [ ] A `ConfigScopeUser` write still cannot carry a base layer (`user.go:37`).
- [ ] The `harbortest/devstack` twin (`devstack.go:404`, `:653`, `:780`, `:961`) carries the
      same behaviour, so the two binaries cannot drift (§17.6).
- [ ] `docs/plans/README.md:358`'s two false claims (the nonexistent registry lookup; the
      `projection.go:186` citation) are corrected in this PR (§4.2 item 11).
- [ ] **This phase OWNS `wire-manifest.gen.json` and `docs/site/protocol/*.md` for the
      wave.** `make protocol-ts-gen` and `make protocol-docs-gen` are run here and land in
      Stage 1; phases 214 and 216 rebase on this PR's regenerated output rather than
      regenerating in parallel. Hand-editing either is rejection-on-sight (§13, D-209,
      D-223), so a merge conflict in them cannot be hand-resolved — the loser must re-run
      the generators.
- [ ] Mutation-verified: reverting the edge validation turns a smoke `OK` into a `FAIL`.
- [ ] `scripts/smoke/phase-215.sh` passes against the preflight dev server with OK > 0 and
      FAIL = 0.

## Files added or changed

```text
internal/protocol/
├── types/control.go          # StartRequest.AgentID (additive, omitempty)
├── protocol.go               # the AgentResolver seam + its Option; the
│                             #   fail-closed unwired posture
├── control.go                # dispatchStart: edge validation before Spawn,
│                             #   carry-through onto SpawnRequest
└── control_agent_test.go     # NEW — the validation table + D-025 N=128

internal/tasks/
└── tasks.go                  # SpawnRequest.AgentID + Task.AgentID
                              #   (whole-record marshal, no migration —
                              #    the OutputSchema precedent, tasks.go:190-209)

internal/runtime/serve/
├── runloop.go                # the per-run effective agent id: tasks.Get moves
│                             #   above reconcileConnections (:840/:849); the
│                             #   eleven projection reads take it; :1285 UNCHANGED
├── mux.go                    # wires the agent resolver onto the control surface
│                             #   from the same AgentConfig registry + AgentConfigID
│                             #   it already holds (:107, :384)
└── runloop_agent_selection_test.go  # NEW — incl. the credential-plane pin

harbortest/devstack/devstack.go      # the twin (§17.6)

internal/tasks/protocol/             # TaskRow.agent_id projection (task-scoped only)

web/console/src/lib/protocol/
├── control.ts                       # hand-mirrored wire field (D-223)
└── wire-manifest.gen.json           # REGENERATED — OWNED BY THIS PHASE

test/integration/agent_selection_test.go   # NEW
scripts/smoke/phase-215.sh                 # the live gate
docs/decisions.md                          # D-360
docs/glossary.md                           # the two new terms
docs/plans/README.md                       # status flip + the two corrections
docs/site/protocol/{methods,types}.md      # REGENERATED — OWNED BY THIS PHASE
docs/site/protocol/task-control.md         # the hand-written choreography guide
docs/skills/use-the-harbor-protocol/SKILL.md   # surface: protocol (§18)
```

Deliberately NOT in the list: `internal/sessions/` (the D-309 departure — the session
projector is untouched) and `internal/protocol/bodyscope/` (`StartRequest` is already
registered at `coverage.go:82`).

## Public API surface

```go
// internal/protocol/types
type StartRequest struct {
    // ...existing fields...

    // AgentID names which agent's configuration the run executes under.
    // OPTIONAL: omitted binds the runtime's configured default exactly as
    // before. Accepted when it equals the runtime's configured default id,
    // or when a config revision exists for the caller's tenant; anything
    // else is refused, never substituted — a caller that named A and
    // silently got B was told it succeeded, which is the defect this field
    // closes. It selects CONFIGURATION only: the run's southbound
    // provenance and its RFC 8693 acting principal remain the runtime's
    // boot-derived value and are never influenced by this field.
    AgentID string `json:"agent_id,omitempty"`
}

// internal/protocol
//
// AgentResolver answers whether a caller-named agent may execute on this
// runtime. The contract is the two-check rule: the id equals the runtime's
// configured default, OR a ConfigScopeAgent revision exists for
// (caller tenant, agentID). Both "registered to another tenant" and "never
// existed" answer false identically — the config key is tenant-scoped by
// construction, so the refusal is not an existence oracle.
//
// Defined consumer-side (like SessionEnsurer) so internal/protocol does not
// import internal/agentcfg. A surface built WITHOUT a resolver REFUSES any
// non-empty agent_id — it does not accept it and does not ignore it.
type AgentResolver interface {
    ResolveAgent(ctx context.Context, id identity.Identity, agentID string) (bool, error)
}

func WithAgentResolver(r AgentResolver) Option

// internal/tasks
type SpawnRequest struct {
    // ...existing fields...
    // AgentID is the caller-named agent, validated at the Protocol edge
    // before the spawn. Empty means the runtime's configured default.
    AgentID string
}

type Task struct {
    // ...existing fields...
    // AgentID is the agent this task's run executes under, persisted via
    // the whole-record marshal. Empty means the runtime's configured
    // default (the pre-phase behaviour, and every historical row).
    AgentID string `json:",omitempty"`
}
```

## Test plan

- **Unit** (`internal/protocol/control_agent_test.go`): the validation table — empty ⇒
  accepted and the spawn carries ""; the configured default id with NO revision ⇒
  accepted (check (i), the case registry-membership validation would have failed);
  an id with a `ConfigScopeAgent` revision ⇒ accepted (check (ii)); an unknown id ⇒
  refused `CodeInvalidRequest` **with `tasks.Spawn` never called** (a recording registry
  asserts zero spawns); a foreign tenant's id ⇒ refused with a byte-identical error to the
  unknown case; a non-empty id against a surface with NO resolver ⇒ refused; an empty id
  against the same surface ⇒ accepted. Carry-through from `StartRequest` → `SpawnRequest`
  → the persisted `Task`.
- **Unit** (`internal/runtime/serve/runloop_agent_selection_test.go`): the per-run
  effective agent id drives each of the eleven projection reads (table-driven over the
  projection functions, two agents with divergent revisions); a run whose task carries no
  `AgentID` resolves the boot value; **the credential-plane pin** — a run whose task names
  agent B under a runtime booted as agent A asserts `tools.InvokingAgentFrom(runCtx) == A`,
  that the MCP `_meta` builder emits `agent_id: A`, and that the token-exchange form
  carries `actor_token = A`. Mutation-verified: threading the named value into
  `runloop.go:1285` must fail this test.
- **Integration** (`test/integration/agent_selection_test.go`, §17.1 — this phase consumes
  the agent-config registry's surface and closes the edge↔run-loop seam): real
  StateStore-backed `agentcfg.Registry`, real task registry, real control transport.
  Register two agents under one tenant with different prompt layers and different skill
  membership; start a run naming each and assert the resolved prompt and catalog view
  differ and match the named agent. Start a run naming none and assert parity with the
  pre-phase default. **Failure modes (≥2, §17.3):** (1) a foreign tenant's agent id is
  refused AND no task row was written; (2) a resolver whose backing store errors fails the
  request loud (`CodeRuntimeError`) rather than falling through to the default — the §13
  posture, and the one place a "resolve error ⇒ allow" would hide. Identity propagation is
  asserted end-to-end (the triple reaches the config read). A `ConfigScopeUser` layer under
  the named agent composes in the documented order.
- **Conformance:** N/A — no persistence interface gains a method. `Task.AgentID` rides the
  existing whole-record marshal on every driver (the `OutputSchema` precedent,
  `tasks.go:198-209`); a durable round-trip assertion rides the existing task-driver
  conformance suite.
- **Concurrency / leak (D-025, §11):** N≥128 concurrent `start` calls against ONE shared
  `ControlSurface` and ONE shared `RunLoopDriver` under `-race`, across two agents × two
  tenants, asserting (a) no data race, (b) each run resolves ITS OWN agent's config —
  content-checked per goroutine, not just "no panic", (c) cancelling one run's ctx does not
  disturb another's, (d) `runtime.NumGoroutine` returns to baseline after teardown. The
  shape matters: N runs against N drivers would prove nothing — the bug this guards is a
  per-run agent id parked on the shared driver.

## Smoke script additions

`scripts/smoke/phase-215.sh` — `live-server` (it drives the booted dev server), with the
404/405/501 → SKIP convention across the block:

1. `start` with no `agent_id` succeeds (200) and a `tasks.get` read-back reports an empty
   `agent_id` — the unchanged path.
2. `start` naming the runtime's configured default id succeeds and the task read-back
   reports it — check (i), the case a registry-membership rule would have refused.
3. `agent_config.set_revision` under a SECOND agent id, then `start` naming it → 200, and
   the task read-back reports that id — check (ii).
4. `start` with an unknown `agent_id` → 400 `invalid_request`, **and no task was created**,
   asserted by a `tasks.list` count taken before and after (the "refused before the task
   exists" property, which a status-code-only assertion would not catch).
5. `start` with an `agent_id` registered under another tenant → the response body is
   byte-identical to step 4's (the non-oracle property).
6. A static guard that `internal/runtime/serve/runloop.go`'s `tools.WithInvokingAgent`
   call site still passes `d.agentConfigID` and not a task-derived value — the Ruling A
   trip-wire, cheap and mechanical, so a later "unification" refactor fails preflight.
7. `go test -race` gates on the edge-validation suite, the run-loop selection suite
   (including the credential-plane pin) and the integration test.

## Coverage target

Targets are the **measured** baselines on `plan/v124-wave`, taken with
`go test -cover ./internal/protocol/ ./internal/tasks/... ./internal/runtime/serve/`
(2026-07-29) — floors, never a sanctioned regression:

| package | measured | target |
|---|---|---|
| `internal/protocol` | 77.9% | **80%** — the phase adds a well-covered edge-validation path, so this improves toward the repo's 85% norm; 77.9% is the hard floor a PR may not drop below |
| `internal/tasks` | 87.2% | **87%** (no-regression; the additions are two struct fields exercised by the round-trip tests) |
| `internal/runtime/serve` | 84.4% | **85%** (the new selection path is fully covered; this closes the last 0.6%) |

`internal/runtime/agentcfg/projection` (measured 81.0%) is NOT listed: every projection
already takes `agentID` as an ordinary parameter, so the package gains no lines.

## Dependencies

- **206** — the owner-scoped registry whose `auth.Owner{Tenant, Agent}` partitioning
  (`projection.go:184`, `:259`, `:334`, enforced at
  `internal/tools/drivers/mcp/registry.go:550-552`) makes concurrent multi-agent reconcile
  safe. Without it, two runs under different agents in one process would thrash each
  other's connections — this phase makes that concurrency reachable for the first time.
- **211** — the sibling owner-scoped mutators, for the same reason.

## Risks / open questions

- **RESOLVED (was an open question): what can a Console selector actually offer today?**
  `agents.list` already ships — `methods.MethodAgentsList` at
  `internal/protocol/methods/methods.go:872`, route `POST /v1/agents/list`, glossary entry
  at `docs/glossary.md:134`. So §13's "no Console page without its feeding Protocol
  surface" is satisfied without new work. **But the answer is narrower than it looks:**
  `RegistryProjector.ListAgents` (`registry_projector.go:253-272`) calls `p.reg.List(ctx)`,
  which is triple-scoped (`registry_impl.go:295` → `requireIdentity:580`), and then appends
  the SYNTHETIC `IsDefault` row for the boot agent (`:246`, `:268-270`). Under a NEW
  session — which is the state a Console selector is rendered in — the registry read
  returns zero rows, so `agents.list` returns exactly one row: the synthetic default.
  **A selector built on `agents.list` today can therefore offer only the default agent**,
  which is also the agent check (i) accepts. That is coherent (the selector offers what the
  runtime will honour) but it means the selector becomes USEFUL only once a consumer also
  enumerates configured agents. A tenant-scoped "agents that have a config revision"
  listing is the natural successor surface; it is named here as a follow-up, deliberately
  not built, and no Console work is in this phase.
- **The default agent is unregistered, and that stays true.** `mux.go:378-381` names it;
  this phase does NOT register it at boot (that would change fleet-listing semantics for
  every runtime as a side effect of a control-surface change). Check (i) is the explicit,
  documented exemption instead of a silent bypass — the failure mode the first draft
  flagged ("silently exempting the default would leave the validation asserting less than
  it appears to") is closed by making the exemption a named rule with its own test.
- **Sizing was revised upward once and then back down; the record matters.** An earlier
  reading held that the projections were per-runtime and would need restructuring. They are
  not — all eleven take `agentID` as an ordinary parameter
  (`projection.go:167, 245, 320, 379, 484, 572, 640, 699, 830, 881, 911`) — so that layer is
  a parameter thread. The genuinely stateful reconcile legs were ALREADY partitioned by
  `auth.Owner{Tenant, Agent}`. **The work is concentrated at the edge (validation) and the
  boundary (carry-through), not in the projections.** Preserved so the next author does not
  re-inflate the estimate.
- **Wave-level: generated-file ownership.** This phase owns `wire-manifest.gen.json` and
  `docs/site/protocol/*.md` in Stage 1 (`docs/plans/wave-v124-coordination.md:80-84`).
  Phases 214 and 216 also touch them and must rebase rather than regenerate in parallel.
- **No open RFC question gates this phase.**

## Glossary additions

- **Caller-named agent** — an agent explicitly identified on a run-start request
  (`StartRequest.agent_id`), as opposed to the runtime's configured default. Accepted only
  under the two-check rule; refused loudly otherwise, never substituted. Selects
  CONFIGURATION only — the run's southbound provenance and RFC 8693 acting principal stay
  boot-derived (D-360).
- **Agent-selection two-check rule** — the edge validation a caller-named agent passes: its
  id equals the runtime's configured default agent id, OR a `ConfigScopeAgent` revision
  exists for `(caller tenant, agent id)`. Neither the agent registry nor a base revision is
  consulted: registry membership would refuse the unregistered boot agent
  (`serve/mux.go:378-381`) and the only tenant-scoped registry read is an admin-gated
  full-store scan; and `agentcfg.Registry.Active` returns `(zero, false, nil)` for an agent
  nobody has configured, which is the default agent's normal state
  (`internal/agentcfg/agentcfg.go:555-557`). D-360.

The existing **Agent provenance** entry (`docs/glossary.md:18`) is reconciled with the two
carriers Ruling A separates: it gains a sentence stating that the provenance `agent_id` is
boot-derived and is NOT the caller-named selection field.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] `make protocol-ts-gen-check` and `make protocol-docs-gen-check` pass; the regenerated
      manifest and site pages are committed here (this phase owns them for the wave)
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ the measured floors above
- [ ] Cross-session AND cross-tenant isolation tests pass
- [ ] **Concurrent-reuse test passes** — N≥128 concurrent starts across two agents × two
      tenants against ONE shared surface and ONE shared driver under `-race`, asserting
      per-run config resolution, no cancellation cross-talk, and a goroutine baseline
- [ ] **Integration test exists** — real drivers on the seam, identity propagation
      asserted, ≥2 failure modes (foreign-tenant refusal with no task row; a resolver
      store error failing loud rather than defaulting), under `-race`
- [ ] The credential-plane pin passes and the static smoke guard on `runloop.go:1285` is in
      place
- [ ] `docs/plans/README.md`'s Phase 215 row is flipped to `Shipped` and its two false
      claims are corrected (§4.2 item 11)
- [ ] Glossary updated (two new terms + the Agent-provenance reconciliation)
- [ ] Departures justified above + `docs/decisions.md` D-360 filed
- [ ] Operator skills updated (§18) — `use-the-harbor-protocol` (surface: protocol) and the
      hand-written `docs/site/protocol/task-control.md` choreography guide
