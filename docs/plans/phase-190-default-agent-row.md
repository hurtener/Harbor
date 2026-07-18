# Phase 190 — agents.list surfaces the synthetic default agent

## Summary

`agents.list`'s registry scopes over agents explicitly registered by
session orchestration (`internal/runtime/registry`); a runtime serving
only its synthetic boot agent — the one every `harbor dev` / `harbor
serve` / generated-binary process boots with, never registered as a
fleet entity — has produced zero rows for every such runtime since
Phase 73e shipped. This phase emits that agent as a first-class,
additively-marked `agents.list` row (`is_default: true`) so a fleet
Agents catalog composing reads across runtimes sees "one agent, not
enumerable this way" instead of an indistinguishable empty page.

## RFC anchor

- RFC §6.16
- RFC §5.2
- RFC §7

## Briefs informing this phase

- brief 11
- brief 05

## Brief findings incorporated

- brief 11 §"Agents view" (OVERVIEW): "List of configured agents in the
  current tenant... for a single-agent setup, you see one row" —
  `docs/skills/observe-with-the-console/SKILL.md` §"Agents — the
  registry" already states this as the intended UX; this phase is what
  makes it literally true for a runtime with zero fleet-registered
  sub-agents (today it renders zero rows, not one).
- brief 11 §"Open questions": "Agent as a Protocol-addressable
  principal. The Agents view requires agents to exist on the Protocol
  with addressable IDs" — the synthetic row is the minimal instance of
  this: the runtime's own boot agent gets a stable, addressable
  `agent_id` on the wire without requiring an explicit `registry.Register`
  call nobody issues for the common single-agent deployment.
- brief 05 (Sessions / identity model): "Identity is the triple
  `(tenant_id, user_id, session_id)`" — the synthetic row's per-row
  `Identity` attribution is derived from this same triple, never from a
  registration event the default agent never had.
- D-311 (class rule, quoted from `docs/decisions.md`): "For any read
  surface where a value may be absent: (1) make the absence
  REPRESENTABLE in-band... so a consumer can tell 'we don't have this'
  from 'the value is zero / the set is empty'... Honest zeros are
  acceptable ONLY when they are documented... and no facet / sort /
  filter silently operates over them as if they were known values."
  This phase is a new instance of the class: an empty `agents.list` on
  an actively-serving runtime is the exact "absence read as zero"
  failure D-311 names.

## Findings I'm departing from (if any)

None.

## Goals

- A runtime whose Agent Registry holds zero fleet-registered agents,
  but which is actively serving traffic through its boot-configured
  agent, returns exactly one honest `agents.list` row for it instead of
  an empty page.
- The row is distinguishable from a registered sub-agent via an
  additive `is_default: true` wire marker — a consumer never confuses
  "the runtime's own agent" with "a registered fleet member."
- `agents.get` on the synthetic row's well-known id resolves it (no
  404) so a Console click-through / drill-down composes with the list
  row.
- The existing admin-widened fleet fan-in (`agents.list` with
  `filter.tenant_ids`, `internal/runtime/registry/protocol/aggregating_projector.go`)
  picks up the synthetic row with no bespoke fan-in code — proven by an
  integration test, not asserted by inspection.
- Scope/authority is unchanged: server-derived from the verified
  session per D-299, never the request body; no new identity axis; no
  scope-gate relaxation.

## Non-goals

- No change to sub-agent registration semantics (`registry.Register` /
  `Deregister` / the three-ID model, D-059/D-060) — the synthetic row is
  never written to the Agent Registry's StateStore.
- No fleet-control verbs (`agents.pause` / `.drain` / `.restart` /
  `.force_stop` / `.deregister`) against the synthetic row's well-known
  id. It falls out for free: `Controller` is still the real
  `*registry.Registry`, which has no record for the id, so a control
  call against it fails closed with the existing `ErrAgentNotFound` —
  this phase adds no new control-verb branch and does not change that
  outcome.
- No wiring of the (still-unwired, out of scope) `ConfigSource` join —
  `agents.tools` / `.memory` / `.governance` / `.skills` on the synthetic
  row stay honest empty projections, exactly as they already are for a
  real registered agent when no `ConfigSource` is supplied
  (`internal/runtime/registry/protocol/registry_projector.go`).
- No `version_hash` computation for the synthetic row. The default
  agent's real content-addressed configuration lives in
  `internal/agentcfg` (the desired-state registry, keyed by the same
  well-known agent id) and is exposed via `agent_config.*` — this phase
  does not compute or fabricate a registry-shaped `version_hash` for a
  record that was never registered.
- No new identity axis, no scope-gate change, no `ProtocolVersion` bump.

## Acceptance criteria

- [ ] `agents.list` (non-widened, caller's own identity scope) returns
      exactly one `Agent` row with `is_default: true` and a stable,
      well-known `id` when the Agent Registry holds zero records for
      the caller's scope.
- [ ] The synthetic row's `Identity` carries the caller's own verified
      `(tenant, user, session)` triple for a non-widened read — the
      default agent is serving THAT call under THAT scope.
- [ ] Collision rule enforced: if a real `AgentRecord` is registered
      under the same well-known id (an operator re-registers a
      sub-agent reusing it), the registered record's row wins and NO
      synthetic row is emitted — one row per id, real data over a
      synthetic placeholder, never a duplicate `id` in one response.
- [ ] `agents.get {id: <well-known default id>}` resolves the synthetic
      projection (not `ErrAgentNotFound`) when no colliding real
      registration exists.
- [ ] `agents.metrics`'s `Active` counter includes the synthetic row
      when it is emitted (status is always `active` — the call
      succeeding proves the runtime is serving).
- [ ] The admin-widened fleet fan-in (`Filter.TenantIDs` +
      `auth.ScopeAdmin`) surfaces one synthetic row per named tenant
      this runtime serves, alongside real registered rows, through the
      unchanged `RegistryProjector.ListTenantAgents` seam — proven by
      an integration test that issues a widened read and asserts the
      marked row is present, not by code inspection alone.
- [ ] A fleet-control verb (`agents.pause` etc.) against the well-known
      default id fails with the existing not-found error — no new
      error path, no accidental control surface over the runtime's own
      process.
- [ ] `is_default` is additive on the `Agent` wire type; `agents.list`'s
      response shape is otherwise byte-compatible; `ProtocolVersion`
      stays unchanged.
- [ ] Full D-223 lockstep: `wire-manifest.gen.json` regenerated
      (`make protocol-ts-gen`), the hand-maintained
      `web/console/src/lib/protocol/agents.ts` `Agent` interface gains
      `is_default: boolean`, `make protocol-ts-gen-check` passes.
- [ ] Full D-209 lockstep: `make protocol-docs-gen` regenerates
      `docs/site/protocol/types.md` (the `Agent` type's new field row).
- [ ] Console Agents catalog (`web/console/src/routes/(console)/agents/+page.svelte`)
      renders an `is_default` badge/pill on the synthetic row,
      distinguishing it visually from a registered sub-agent card.
- [ ] `docs/skills/observe-with-the-console/SKILL.md` §"Agents — the
      registry" and `docs/skills/use-the-harbor-protocol/SKILL.md`
      (the `agents.list` / fleet-enumeration passage) are updated to
      name the `is_default` marker where they describe `agents.list`.
- [ ] `internal/tui` is confirmed to have no `agents.*` consumption
      (grepped, not assumed) — this phase adds none; the TUI is a
      conversation surface over sessions/tasks, not the Agents catalog.

## Files added or changed

```text
internal/runtime/registry/protocol/registry_projector.go   # default-agent synthesis + collision rule
internal/runtime/registry/protocol/aggregating_projector.go # fleet fan-in picks up the same synthesis
internal/runtime/registry/protocol/service.go               # (only if List's dedupe/sort needs a touch)
internal/protocol/types/agents.go                            # Agent.IsDefault bool `json:"is_default,omitempty"`
internal/runtime/serve/mux.go                                # constructs + injects the default-agent descriptor
internal/runtime/registry/protocol/registry_projector_test.go
internal/runtime/registry/protocol/fleet_test.go
test/integration/agents_page_test.go                          # extended: default row + widened fan-in + collision
web/console/src/lib/protocol/agents.ts                        # Agent.is_default
web/console/src/lib/protocol/wire-manifest.gen.json            # regenerated
web/console/src/routes/(console)/agents/+page.svelte           # is_default badge
docs/site/protocol/types.md                                    # regenerated (Agent type)
docs/skills/observe-with-the-console/SKILL.md                  # Agents section update
docs/skills/use-the-harbor-protocol/SKILL.md                   # agents.list passage update
docs/glossary.md                                               # default agent, synthetic agent row
scripts/smoke/phase-190.sh
```

## Public API surface

```go
// internal/protocol/types/agents.go — additive field on the existing
// catalog-row wire type.
type Agent struct {
    // ... existing fields unchanged ...

    // IsDefault marks the runtime's synthetic default agent — the one
    // serving the identity scope it was booted with, never written to
    // the Agent Registry's StateStore. false (the zero value, omitted
    // on the wire) for every real registered agent. A consumer uses
    // this to distinguish "the runtime's own agent" from a registered
    // fleet member; it never widens or narrows the isolation tuple.
    IsDefault bool `json:"is_default,omitempty"`
}
```

```go
// internal/runtime/registry/protocol/registry_projector.go — the
// caller-supplied descriptor + the option that wires it. Optional:
// a nil/unset descriptor means "no default agent to synthesize" (e.g.
// an embedder with no boot AgentConfig) — the projector's existing
// behavior is unchanged, matching the ConfigSource nil-is-honest
// pattern already on this type.
type DefaultAgentDescriptor struct {
    ID          string // the well-known boot agent id
    DisplayName string
    PlannerType string
    Model       string
    BootedAt    time.Time
}

func WithDefaultAgent(d DefaultAgentDescriptor) ProjectorOption
```

## Test plan

- **Unit:** `registry_projector_test.go` — synthetic row present with
  `IsDefault:true` when the registry is empty for the caller's scope;
  suppressed (collision rule) when a real record shares the well-known
  id; `GetAgent` resolves the synthetic id; absent entirely when
  `WithDefaultAgent` is not supplied (existing behavior unchanged,
  regression guard). `service_test.go` — `agents.metrics` Active
  counter includes the synthetic row; pagination/sort/facet filters
  operate over it like any other row (no special-cased bypass).
- **Integration:** `test/integration/agents_page_test.go` extended —
  real `*registry.Registry` (StateStore-backed) with zero registrations,
  a real wire round-trip through the `agents.*` handler, asserting the
  synthetic row on the narrow read; the admin-widened fan-in
  (`Filter.TenantIDs` + verified `auth.ScopeAdmin`) over ≥2 tenants
  asserting one synthetic row per tenant alongside any real registered
  rows; identity propagation (the row's `Identity` matches the caller's
  verified triple on the narrow path); ≥1 failure mode (a fleet-control
  verb against the well-known id still fails `ErrAgentNotFound` — proves
  the non-goal boundary holds, not just the happy path).
- **Conformance:** N/A — `agents.*` methods are explicitly `t.Skip`'d in
  `internal/protocol/conformance/conformance.go` (line ~999, "agents.*
  methods exercised by internal/runtime/registry/protocol tests +
  stream agents_handler tests + test/integration/agents_page_test.go");
  this phase's coverage lands in those same suites, not a new
  conformance scenario.
- **Concurrency / leak:** `fleet_test.go` / `concurrent_reuse_test.go`
  extended — N≥100 concurrent `ListAgents`/`ListTenantAgents` calls
  against one `RegistryProjector` configured with `WithDefaultAgent`,
  under `-race`, asserting the synthetic row is stable per-call (no
  shared mutable descriptor state leaking across calls — the descriptor
  is immutable after `NewRegistryProjector`, per the existing
  concurrent-reuse contract this type already satisfies).

## Smoke script additions

`scripts/smoke/phase-190.sh` (live-server class):

- `POST /v1/agents/list` against the running dev server with a fresh
  identity scope (no registered agents) returns HTTP 200 with
  `agents[].is_default == true` for at least one row —
  `skip_if_404`/`skip_if_405` convention if the surface predates this
  phase's build.
- Static grep: `Agent.IsDefault` / `is_default` present in
  `internal/protocol/types/agents.go` and
  `web/console/src/lib/protocol/agents.ts` (lockstep sanity, cheap
  pre-check before the live assertion).

## Coverage target

- `internal/runtime/registry/protocol`: 85%
- `internal/protocol/types` (touched lines): 85%

## Dependencies

- 184

## Risks / open questions

- The well-known default-agent id is reused, not newly minted: this
  phase threads whatever boot id `internal/runtime/serve/serve.go`
  already assigns (the constant already flows into `MuxInput.AgentConfigID`
  and is consumed elsewhere, e.g. the MCP Apps accessor's `AgentID`). No
  rename is proposed — renaming that constant has a wider blast radius
  than this phase's scope and is not required to satisfy the ask.
- The admin-widened row's `Identity` cannot carry a single owning
  session (the default agent serves every session on the runtime, not
  one) — it carries `{Tenant: <named tenant>}` with `User`/`Session`
  left empty. This is deliberately representable-absence (D-311), not a
  fabricated session id; the acceptance criteria and integration test
  pin this shape so it does not drift into a fabricated value later.
- A future phase that wires a production `ConfigSource`
  (`registry_projector.go`'s `WithConfigSource`, currently unwired for
  ALL agents — a separate, already-known gap this phase does not close)
  should confirm it degrades honestly for the synthetic row too (no
  `GetAgent`-style registry lookup to join against). Flagged here, not
  fixed here — out of this phase's stated scope.

## Glossary additions

- default agent
- synthetic agent row

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A, no isolation-tuple change; the row-attribution test above covers identity propagation.
- [ ] **If this phase builds a reusable artifact...concurrent-reuse test passes** — `RegistryProjector` is an existing reusable artifact this phase extends; the concurrent-reuse test above covers the new descriptor field under N≥100 `-race`.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists...** — yes, `test/integration/agents_page_test.go` extended per the Test plan above, real StateStore-backed registry, identity propagation, ≥1 failure mode, `-race`.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — N/A, no departure; this phase's decision (D-327) is pre-assigned and already stubbed in `docs/decisions.md`.
