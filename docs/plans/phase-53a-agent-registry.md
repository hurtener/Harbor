# Phase 53a — agent-registry

## Summary

Phase 53a lands `internal/runtime/registry` — the **Agent Registry**: an
in-process, per-runtime-instance subsystem that owns the *registration
identity* of agents. It implements the three-ID model (`agent_id` /
`incarnation` / `version_hash`, D-059), persists records through the
instance's configured `state.StateStore` (in-mem / SQLite / Postgres,
§4.4 seam — the registry consumes the existing StateStore seam, it does
not add a new driver seam of its own), handles both creation cases
(locally-hosted + connect-to-remote handle, D-060), emits `agent.*`
events on the typed `events.EventBus` so the Console Agents page is a
runtime lens (D-061/D-062), and gates fleet-control commands behind a
more-elevated privilege tier than fleet observation with audit-redacted
emission (D-066).

## RFC anchor

- RFC §6.16
- RFC §7

## Briefs informing this phase

- brief 09
- brief 11

## Brief findings incorporated

- **brief 09 §"Agent identity (Harbor's addition — bifrost has no
  concept)":** "Agent-bound OAuth tokens are keyed by `agent_id`, not by
  a user identifier." Phase 53a is the subsystem that mints and persists
  that `agent_id`; the registry's `AgentID` is the exact key Phase 30
  will use for `(tenant, agent_id, source)` agent-bound token lookups.
  The registry therefore exposes `agent_id` as a stable, rehydrated-on-
  restart value — Phase 30 cannot key tokens against an id that changes
  every boot.
- **brief 09 §"Server-level / agent-bound flow":** the agent is a
  first-class addressable subject configured *once during agent setup*,
  distinct from the per-chat user identity. Phase 53a models this: an
  `AgentRecord` is registered explicitly (by operator wiring / CLI /
  `harbor dev`), not implicitly minted per request. Registration is a
  deliberate lifecycle event, not a side effect of a run.
- **brief 11 §"Agents view (OVERVIEW)":** the Agents page lists
  "configured agents in the current tenant" with per-agent operational
  fields (planner, tool attachments, health) and a control surface.
  Phase 53a provides the runtime-side source of truth that page renders:
  `agent.*` events + a `List` snapshot scoped by identity. The brief's
  own checklist item — "Agent-identity RFC stub landed" — is satisfied by
  RFC §6.16; this phase is its implementation.
- **brief 11 §"`Agents ≠ chatbots`" (and RFC §7.2):** "Agents are runtime
  execution entities ... not personas." The registry stores operational
  registration identity (ids, planner config descriptor, health, drain
  state) — never persona/prompt content as a user-facing object. Prompt
  *content* feeds `version_hash` as hashed input only.
- **brief 11 §"elevated view":** fleet-wide observation requires an
  elevated subscription; Phase 53a extends this to a *second, higher*
  tier for fleet **control** (D-066) — observation and control are
  distinct scope claims, control commands are audit-redacted and emitted.

## Findings I'm departing from (if any)

None. brief 09 raised an open RFC-level question — "does Harbor's
identity surface extend to a quadruple `(tenant, agent, user, session)`"
— and recommended making `agent_id` a peer of the isolation tuple. That
recommendation was **explicitly rejected** at RFC time and settled by
D-059: `agent_id` is a registration identity, not an isolation principal.
Phase 53a follows D-059 (the settled decision), not brief 09's superseded
recommendation. This is not a silent departure — D-059 already records
the resolution; this section notes it so the brief→RFC delta is visible.

## Goals

- A `registry.AgentRegistry` interface + one concrete `*Registry`
  implementation, StateStore-backed, per-runtime-instance, in-process.
- The three-ID model: `agent_id` (ULID, minted once at first
  registration, persisted, rehydrated), `incarnation` (bumps every
  process start / every `Register` of an already-known agent),
  `version_hash` (deterministic content hash; bumps iff configuration
  content changed).
- Both creation cases: `Register` for locally-hosted agents (runtime
  mints a local `agent_id`); `RegisterRemote` for connect-to-remote
  agents (local `agent_id` is a *handle*; an `AgentCardRef` points at the
  remote operator's canonical A2A AgentCard).
- `restart ≠ recreate`: re-registering a known logical agent (same
  `RegistrationKey`) rehydrates the existing record and bumps
  `incarnation`; `Deregister` then `Register` mints a fresh `agent_id`.
- `agent.*` events (`agent.registered`, `agent.restarted`,
  `agent.health`, `agent.drained`, `agent.deregistered`) on the
  `events.EventBus`, carrying the registration `agent_id`.
- Fleet-control surface (`Pause` / `Drain` / `Restart` / `ForceStop`)
  gated behind an elevated control-scope claim; every control command is
  audit-redacted and emitted. Fleet observation (`Get` / `List` /
  `Inspect`) requires only the ordinary identity scope.
- Identity is mandatory and fails closed: every method rejects a missing
  / incomplete identity triple. `agent_id` is **never** used as a
  `WHERE`-clause isolation filter — storage scopes by
  `(tenant, user, session)` only.
- Concurrent-reuse safety (D-025): one shared `*Registry` is safe under
  N≥100 concurrent registrations / lookups / control commands.

## Non-goals

- **No Protocol surface.** `agent.*` events and the registry snapshot
  reach the Console through the Protocol phases (54+); Phase 53a ships
  the runtime subsystem only. No REST endpoint, no Protocol method.
- **No new StateStore driver.** The registry persists through the
  *existing* `state.StateStore` seam (D-027). It does not define a
  `registry/drivers/` seam — driver pluralism already lives at the
  StateStore layer (in-mem / SQLite / Postgres).
- **No agent execution.** The registry owns registration *identity* and
  *lifecycle metadata*, not planner runtime, tool dispatch, or task
  ownership. Wiring the registry into the run path is later work.
- **No cryptographic scope verification.** The elevated control-scope
  claim is trust-based in Phase 53a (mirrors the events package's
  Phase-05 `Admin` claim); cryptographic verification arrives with
  Protocol auth (Phase 61).
- **No control-plane client enrollment allowlist.** Deferred per D-066
  ("decide later").
- **No Console page.** The Agents page is Console-wave work (72–75
  re-decomposition); it ships only after this phase's feeding Protocol
  surface (D-062).

## Acceptance criteria

- [ ] `agent_id` is stable across a process restart when a durable
  StateStore driver (SQLite / Postgres) is configured — a rehydration
  test registers an agent, "restarts" by constructing a fresh
  `*Registry` over the same store, and asserts the same `agent_id`.
- [ ] The in-mem driver is documented as dev-only / non-persistent; a
  test asserts a fresh `*Registry` over a fresh in-mem store does not see
  the prior agent (the dev-mode artifact, not the fleet posture).
- [ ] `incarnation` bumps on every re-registration of a known agent
  (every "process start"); `version_hash` is **stable** across a
  re-registration with byte-identical configuration and **bumps** when
  configuration content changes.
- [ ] `version_hash` is deterministic: the same configuration content
  produces the same hash regardless of map ordering or struct field
  enumeration order (canonicalised before hashing).
- [ ] `restart ≠ recreate`: re-registering the same `RegistrationKey`
  keeps the `agent_id` and the StateStore record; `Deregister` followed
  by `Register` of the same `RegistrationKey` mints a *fresh* `agent_id`.
- [ ] Remote-agent registration (`RegisterRemote`) stores a handle
  `agent_id` plus an `AgentCardRef`; the handle is documented and tested
  as runtime-instance-local and never assumed globally unique.
- [ ] Every `agent.*` event carries the registration `agent_id` in its
  payload.
- [ ] Cross-tenant / cross-session isolation conformance: an agent
  registered under `(T1,U1,S1)` is invisible to `Get` / `List` /
  `Inspect` under `(T2,U2,S2)`; one identity's registry view never bleeds
  into another.
- [ ] Fleet-control commands (`Pause` / `Drain` / `Restart` /
  `ForceStop`) require the elevated control-scope claim — a context
  without it is rejected with `ErrControlScopeRequired` — and each emits
  an audit-redacted `agent.*` control event. Fleet-observation methods
  do **not** require the control claim.
- [ ] Missing / incomplete identity fails closed on every method
  (`ErrIdentityRequired`-wrapped) — there is no opt-out knob.
- [ ] Concurrent-reuse test: N≥100 concurrent registrations / lookups /
  control commands against one shared `*Registry` under `-race` — no data
  races, no context bleed (per-run identity asserted), no goroutine
  leaks (baseline `runtime.NumGoroutine` restored after teardown).
- [ ] `scripts/smoke/phase-53a.sh` runs the package tests + the
  integration test under `-race` and shows `OK ≥ 3`, `FAIL = 0`.

## Files added or changed

```text
docs/plans/phase-53a-agent-registry.md         # this plan (new)
docs/plans/README.md                           # 53a row Pending → Shipped
docs/decisions.md                              # D-068 (version_hash algorithm + handle encoding)
docs/glossary.md                               # control-scope claim term
README.md                                      # status table: 53a row
scripts/smoke/phase-53a.sh                     # new smoke script
internal/runtime/registry/registry.go          # AgentRegistry interface, AgentRecord, three-ID model, sentinels, ctx helpers
internal/runtime/registry/registry_impl.go     # *Registry concrete impl over state.StateStore + events.EventBus
internal/runtime/registry/versionhash.go       # deterministic version_hash over agent config content
internal/runtime/registry/events.go            # agent.* EventType constants + SafePayload types + init() registration
internal/runtime/registry/registry_test.go     # unit: three-ID model, restart-vs-recreate, isolation, control-scope, fail-closed, D-025 concurrent reuse
internal/runtime/registry/versionhash_test.go  # unit: version_hash canonicalisation + determinism
test/integration/agent_registry_test.go        # integration: StateStore-backed rehydration across inmem/sqlite/postgres, real EventBus on seam, identity propagation, missing-identity-fails-closed
```

`internal/runtime/registry/` is already listed in CLAUDE.md / AGENTS.md
§3 ("Agent Registry — registration identity + agent.* events"); this
phase fills it. No new top-level directory.

## Public API surface

```go
package registry

// AgentRecord is the persisted registration-identity record for one agent.
type AgentRecord struct {
    AgentID         string             // ULID, minted once, rehydrated on restart
    Incarnation     uint64             // bumps on every process start / re-registration
    VersionHash     string             // deterministic content hash; bumps iff config changed
    RegistrationKey string             // operator-stable logical-agent key; restart != recreate hinges on it
    Identity        identity.Identity  // the (tenant,user,session) the agent is registered within
    Hosting         Hosting            // HostingLocal | HostingRemote
    AgentCardRef    string             // remote only: reference to the canonical A2A AgentCard
    DisplayName     string
    Health          Health             // HealthUnknown | HealthHealthy | HealthDegraded | HealthDraining | HealthStopped
    RegisteredAt    time.Time
    UpdatedAt       time.Time
}

// AgentConfig is the content version_hash is derived from. Caller-supplied
// at Register time; hashed (canonicalised) — never stored as a persona object.
type AgentConfig struct {
    Prompts       []string
    Tools         []ToolDescriptor   // name + schema digest
    PlannerConfig map[string]string
    ModelPolicy   map[string]string
}

// AgentRegistry is the per-runtime-instance registration-identity subsystem.
type AgentRegistry interface {
    // Register mints (or rehydrates) a locally-hosted agent's agent_id.
    Register(ctx context.Context, key string, cfg AgentConfig, opts RegisterOptions) (*AgentRecord, error)
    // RegisterRemote registers a connect-to-remote agent: local agent_id is a handle.
    RegisterRemote(ctx context.Context, key string, cardRef string, opts RegisterOptions) (*AgentRecord, error)
    // Get / List / Inspect — fleet observation; ordinary identity scope.
    Get(ctx context.Context, agentID string) (*AgentRecord, error)
    List(ctx context.Context) ([]AgentRecord, error)
    Inspect(ctx context.Context, agentID string) (*AgentSnapshot, error)
    // ReportHealth updates Health and emits agent.health.
    ReportHealth(ctx context.Context, agentID string, h Health) error
    // Deregister removes the record and emits agent.deregistered. recreate mints fresh.
    Deregister(ctx context.Context, agentID string) error
    // Fleet control — requires the elevated control-scope claim; audit-redacted + emitted.
    Pause(ctx context.Context, agentID string, reason string) error
    Drain(ctx context.Context, agentID string, reason string) error
    Restart(ctx context.Context, agentID string, reason string) error
    ForceStop(ctx context.Context, agentID string, reason string) error
    // Close releases the registry (no long-lived goroutine in V1; symmetry + future-proofing).
    Close(ctx context.Context) error
}

// Control-scope claim helpers (trust-based in V1; crypto verification at Phase 61).
func WithControlScope(ctx context.Context) context.Context
func HasControlScope(ctx context.Context) bool

// Sentinels (callers errors.Is):
var (
    ErrIdentityRequired     = errors.New("registry: identity triple incomplete")
    ErrControlScopeRequired = errors.New("registry: fleet-control requires elevated control-scope claim")
    ErrAgentNotFound        = errors.New("registry: agent not found")
    ErrAgentExists          = errors.New("registry: registration key already active")
    ErrRegistryClosed       = errors.New("registry: registry is closed")
    ErrInvalidConfig        = errors.New("registry: invalid agent config")
)
```

## Test plan

- **Unit:** three-ID model (mint-once `agent_id`, `incarnation` bump on
  re-registration, `version_hash` bump iff config content changed);
  `version_hash` determinism + canonicalisation (map-order independence);
  `restart != recreate` (re-register keeps id; deregister+register mints
  fresh); remote-agent handle stores `AgentCardRef`; every `agent.*`
  event carries `agent_id`; fail-closed on missing identity;
  control-scope gating (`ErrControlScopeRequired` without the claim,
  success with it); audit-redacted control-event emission.
- **Integration** (`test/integration/agent_registry_test.go`,
  `TestE2E_Phase53a_*`): StateStore-backed rehydration across all three
  drivers (inmem — asserts non-persistence; sqlite + postgres — assert
  `agent_id` stable across a simulated restart) wired with a real
  `events.EventBus` (`events.drivers.inmem`) on the seam; identity
  propagation asserted end-to-end (`agent.*` events carry the registering
  triple); ≥1 failure mode — a `Register` call with an incomplete
  identity triple fails closed before touching the store. Real drivers
  everywhere on the seam (no mocks), under `-race`. Postgres leg uses the
  same env-gate convention as the existing Postgres integration tests.
- **Conformance:** cross-tenant / cross-session isolation — an agent
  registered under one triple is invisible to `Get` / `List` / `Inspect`
  under another; N concurrent distinct-identity registrations assert no
  cross-talk in the `List` views.
- **Concurrency / leak:** D-025 concurrent-reuse — N≥100 concurrent
  `Register` / `Get` / `List` / control-command invocations against one
  shared `*Registry` under `-race`; per-invocation identity asserted (no
  context bleed); `runtime.NumGoroutine` baseline restored after
  `Close`.

## Smoke script additions

`scripts/smoke/phase-53a.sh` (no HTTP/Protocol surface yet — correctness
is verified by the Go suite, mirroring `phase-08.sh`):

- `OK`: `go test -race ./internal/runtime/registry/...` passes.
- `OK`: `go test -race -run '^TestE2E_Phase53a_' ./test/integration/...`
  passes.
- `OK`: `go vet ./internal/runtime/registry/...` is clean.
- `skip`: "phase 53a: Agent Registry has no HTTP/Protocol surface yet
  (Console Agents page + Protocol surface land in the 54+ / 72–75 waves)".

## Coverage target

- `internal/runtime/registry`: 85% (master-plan target for this
  conformance-tested subsystem).

## Dependencies

- 01 (identity triple), 05 (`events.EventBus` + `agent.*` event types),
  07 (`state.StateStore` generic surface), 08 (sessions — the
  typed-wrapper-over-StateStore pattern this phase mirrors). All shipped.

## Risks / open questions

- **`version_hash` algorithm choice.** The RFC says "deterministic hash
  over (prompt set, tool set + schemas, planner config, model policy)"
  but does not pin the algorithm or the canonicalisation rule. Phase 53a
  settles this in D-068: SHA-256 over a canonical JSON encoding (sorted
  keys, sorted slices where order is not semantic) of an `AgentConfig`,
  hex-encoded. This is an implementation call within the RFC's envelope,
  not an RFC departure.
- **Handle encoding for remote agents.** D-060 says the local `agent_id`
  for a connect-to-remote agent is a "handle." Phase 53a settles in
  D-068 that the handle is a normal locally-minted ULID `agent_id` with
  `Hosting = HostingRemote` and a non-empty `AgentCardRef` — i.e. the
  handle is not a distinct type, it is the same `agent_id` field
  discriminated by `Hosting`. This keeps the three-ID model uniform
  across both creation cases.
- **No long-lived goroutine.** Unlike `sessions.Registry` (which owns a
  GC sweeper), the Agent Registry has no background loop in V1 — health
  is push-reported via `ReportHealth`, not polled. `Close` is retained
  for interface symmetry and future-proofing. The D-025 leak test still
  asserts baseline goroutine count to catch any accidental goroutine.

## Glossary additions

- `agent_id`, `incarnation`, `version_hash`, `Agent Registry`,
  `Console DB`, `Evaluations` — **already present** in `docs/glossary.md`
  (added at RFC time alongside D-059/060/061/064).
- **New:** `control-scope claim` — the elevated privilege tier that
  fleet-control commands require, distinct from (and higher than) the
  fleet-observation scope (D-066).

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes
- [x] **If this phase builds a reusable artifact: concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`.** `*Registry` is a compiled reusable artifact; `TestRegistry_ConcurrentReuse_D025` ships.
- [x] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists, wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** `test/integration/agent_registry_test.go` wires real state + events drivers.
- [x] If new vocabulary: glossary updated (`control-scope claim`)
- [x] If a brief finding was departed from: justified above + decisions.md entry filed (brief 09 → D-059 already settled; D-068 filed for implementation calls)
