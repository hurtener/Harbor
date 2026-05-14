# Phase 53a — agent-registry

## Summary

Ships the **Agent Registry** — an in-process, per-runtime-instance subsystem that owns the *registration identity* of agents. It mints and persists `agent_id`, tracks the ephemeral `incarnation` and the content-derived `version_hash`, handles both creation cases (locally-hosted and connect-to-remote), and emits `agent.*` events so downstream consumers (tool-side OAuth, the Console Agents page) render runtime state rather than inventing their own.

## RFC anchor

- RFC §6.16
- RFC §7

## Briefs informing this phase

- brief 09
- brief 11

## Brief findings incorporated

- **brief 09 §166–169:** "bifrost has no first-class 'agent' principal … Harbor's Console design treats agents as first-class addressable subjects: `agent_id` appears on every session … Agent-bound OAuth tokens are keyed by `agent_id`, not by a user identifier." This phase makes `agent_id` a real, minted, persisted runtime identifier so Phase 30 can key agent-bound tokens by `(tenant, agent_id, source)`.
- **brief 09 §453:** "First-class agent identity. bifrost has no `agent_id` concept. Harbor's Console-shown agents need to be a runtime principal — addressable, ACL-bound, the owner of agent-scoped OAuth tokens, and captured in the audit trail as the *actor* … This is a cross-cutting addition that Phase 30 *cannot* punt on; it likely warrants an RFC stub before the Phase 30 plan PR." This phase IS that subsystem; the RFC stub is RFC §6.16, settled by D-059/D-060.
- **brief 09 §460:** "Cross-tenant + cross-user + cross-agent isolation conformance … each token resolves only for its owning principal." The registry's conformance suite asserts a registration in one `(tenant, user, session)` scope never resolves for another.
- **brief 11 §169:** "does the stream respect identity-scope filtering inherent in the JWT, or does an admin see every tenant's events by default? Recommendation: respect identity by default, with a per-view 'elevate to fleet view' gesture that requires admin scope." The registry's `List`/observation surface respects the identity scope; fleet-wide visibility requires an elevated scope claim — and fleet *control* requires a higher tier still (D-066).
- **brief 11 §55 + §437:** the operator mockup shows an Agents fleet view with per-agent health, task counts, and runtime controls — confirming the Agents page is a *lens* over a runtime-side registry, not a Console-local list.

## Findings I'm departing from (if any)

- **brief 09 §170** poses the open RFC-level question and recommends `agent_id` become a **peer of the identity triple** — a quadruple `(tenant, agent, user, session)` where isolation predicates may key on `agent_id`. **This phase departs from that recommendation.** D-059 settles `agent_id` as a *registration identity*, **not** an isolation principal: Harbor's isolation boundary stays `(tenant, user, session)` (+ `run`). An agent runs *within* that tuple and does not widen it; storage methods and event filters never key isolation on `agent_id`. Rationale: the acting-subject-vs-requesting-principal provenance need that brief 09 §170 is really chasing is satisfied by recording `agent_id` in provenance and audit (the actor) alongside the identity tuple (the requester) — without making `agent_id` an isolation key, which would force every identity-scoped table and every event filter across the runtime to grow a fourth dimension for a concept that is not an isolation boundary. The departure is recorded in `docs/decisions.md` D-059 per AGENTS.md §15 + §16.

## Goals

- A `registry.AgentRegistry` interface with one V1 implementation, persisted via the runtime instance's configured `StateStore` (the §9 triad — no separate registry-driver seam; persistence rides the existing StateStore seam, mirroring how Governance persists, D-044).
- The three-ID model: `agent_id` (stable, minted once, rehydrated on restart), `incarnation` (ephemeral, bumps every process start), `version_hash` (deterministic content hash, bumps only on configuration change).
- Both creation cases: locally-hosted agents (runtime mints a local `agent_id`) and connect-to-remote agents (local `agent_id` is a handle; canonical identity is the remote A2A AgentCard).
- `restart` rehydrates the same `agent_id` from a durable StateStore; `restart ≠ recreate`.
- `agent.*` events on the typed event bus (`agent.registered`, `agent.restarted`, `agent.health`, `agent.drained`, `agent.deregistered`).
- Fleet-control vs fleet-observation privilege tiers represented in the API surface so downstream Protocol phases can enforce them; every control operation is audit-redacted and emitted.

## Non-goals

- **Protocol exposure.** The registry is a runtime-internal subsystem at this phase. `agents.*` Protocol methods and the Console Agents page land in the Protocol / Console-attaching waves (54+, 72–75) and twin with their own smoke coverage per RFC §7.
- **Agent scaffolding / authoring.** Creating agent definitions via UI is post-V2 (RFC §7.4).
- **A runtime-side control-plane client enrollment allowlist.** Deferred "decide later" per D-066; per-request JWT scope covers V1.
- **Agent-bound OAuth token storage.** That is Phase 30 — this phase only supplies the `agent_id` Phase 30 keys by.
- **Cross-runtime fleet aggregation.** Console-side aggregation, post-V1 (brief 11 §539).

## Acceptance criteria

- [ ] `registry.AgentRegistry` interface defined; one V1 implementation constructed via `registry.New(...)` taking an injected `state.StateStore` + `events.EventBus`.
- [ ] `Register` mints a ULID `agent_id`, persists the agent record via StateStore, and emits `agent.registered`.
- [ ] Three-ID model: `incarnation` bumps on every `New`/boot rehydration; `version_hash` is a deterministic hash over (prompt set, tool set + schemas, planner config, model policy) and is byte-stable across runs given identical configuration content.
- [ ] `restart` rehydration: with a durable StateStore driver, re-constructing the registry yields the *same* `agent_id` for a previously-registered agent, a *new* `incarnation`, and an unchanged `version_hash` when configuration content is unchanged; `agent.restarted` is emitted. With the in-mem driver, the registry starts empty (documented dev-only behaviour, not a silent failure).
- [ ] `restart ≠ recreate`: a `Deregister` + `Register` of the same logical agent mints a fresh `agent_id`; a restart does not.
- [ ] Connect-to-remote: registering a remote agent stores a handle (`agent_id` local to this instance) plus an A2A AgentCard reference; the handle is runtime-instance-local and never assumed globally unique.
- [ ] `agent.*` events carry the registration `agent_id` and the identity tuple; payloads are sealed (`events.Sealed`) and audit-redacted on emit.
- [ ] Identity is mandatory: `Register` / `List` / control operations reject a missing identity component with a wrapped sentinel (fail-closed, §6 rule 9).
- [ ] Cross-tenant / cross-session isolation conformance: a registration in one `(tenant, user, session)` scope never resolves or lists for another.
- [ ] Fleet control vs observation: control operations (`Pause` / `Drain` / `Restart` / `ForceStop`) are gated behind an elevated-scope check and emit audit events; observation (`Get` / `List`) is not. The gate is a typed sentinel, not a silent no-op.
- [ ] Concurrent-reuse test: N≥100 concurrent `Register` / `Get` / `List` / control invocations against one shared `AgentRegistry` under `-race` — no data races, no context bleed (per-goroutine identity round-trips), no cancellation cross-talk, no goroutine leaks (baseline `runtime.NumGoroutine` restored after teardown).
- [ ] `scripts/smoke/phase-53a.sh` exists and passes (skips the Protocol surface, which does not exist until later phases — per the §4.2 404/405/501 → SKIP convention).
- [ ] `cmd/harbor` wires the registry into the runtime boot.
- [ ] `docs/plans/README.md` and root `README.md` status flipped to Shipped in the same PR.

## Files added or changed

```text
internal/runtime/registry/
├── registry.go            # AgentRegistry interface + New(...) constructor + factory wiring
├── agent.go               # Agent, AgentID, Incarnation, VersionHash, RegistrationSpec, AgentCardRef
├── version.go             # version_hash computation (deterministic content hash)
├── store.go               # StateStore-backed persistence (key layout, (de)serialisation)
├── events.go              # agent.* event payloads (sealed via events.Sealed)
├── errors.go              # typed sentinels (ErrIdentityRequired, ErrElevatedScopeRequired, ErrAgentNotFound, ...)
├── registry_test.go       # unit: three-ID model, register/get/list, restart-vs-recreate
├── version_test.go        # unit: version_hash determinism + change-detection
├── d025_test.go           # concurrent-reuse stress (N≥100, -race)
└── conformance/
    └── conformance.go     # cross-tenant/session isolation conformance suite
test/integration/
└── agent_registry_test.go # integration: StateStore-backed rehydration across 3 drivers, real EventBus on the seam, identity propagation, ≥1 failure mode
cmd/harbor/main.go         # wire the registry into runtime boot
scripts/smoke/phase-53a.sh # smoke skeleton (skips: no Protocol surface at this phase)
docs/plans/README.md       # status flip → Shipped
README.md                  # status table flip → Shipped
```

## Public API surface

```go
package registry

// AgentRegistry owns the registration identity of agents for ONE runtime instance.
// It is a reusable artifact (D-025): immutable after construction, safe for N
// concurrent callers; per-call state lives in ctx, never on the registry.
type AgentRegistry interface {
    // Register mints (or, on restart rehydration, restores) an agent_id, bumps
    // incarnation, computes version_hash, persists via StateStore, emits agent.registered
    // (or agent.restarted). Identity-mandatory; fails closed on a partial tuple.
    Register(ctx context.Context, spec RegistrationSpec) (Agent, error)

    // Get / List are observation-tier: scoped by the caller's identity tuple.
    Get(ctx context.Context, id AgentID) (Agent, error)
    List(ctx context.Context, filter ListFilter) ([]Agent, error)

    // Deregister removes the agent record; a subsequent Register of the same logical
    // agent mints a FRESH agent_id (recreate ≠ restart).
    Deregister(ctx context.Context, id AgentID) error

    // Pause / Drain / Restart / ForceStop are fleet-CONTROL tier: gated behind an
    // elevated-scope check, audit-redacted and emitted. ErrElevatedScopeRequired on
    // an insufficient scope claim — never a silent no-op.
    Pause(ctx context.Context, id AgentID) error
    Drain(ctx context.Context, id AgentID) error
    Restart(ctx context.Context, id AgentID) error
    ForceStop(ctx context.Context, id AgentID) error
}

type AgentID string      // stable registration identity; runtime-instance-local ULID
type Incarnation uint64  // ephemeral; bumps every process start
type VersionHash string  // deterministic content hash; bumps only on config change

type Agent struct {
    ID          AgentID
    Incarnation Incarnation
    VersionHash VersionHash
    Identity    identity.Identity // owning (tenant, user, session) scope
    Origin      Origin            // OriginLocal | OriginRemote
    AgentCard   *AgentCardRef     // set iff Origin == OriginRemote (handle → canonical remote identity)
    // ... planner type, tool/memory bindings, health, timestamps
}

// New constructs the V1 registry, persisted via the injected StateStore. On a
// durable driver it rehydrates existing agents (same agent_id, new incarnation).
func New(store state.StateStore, bus events.EventBus, opts ...Option) (AgentRegistry, error)
```

## Test plan

- **Unit:** three-ID model (`agent_id` stable, `incarnation` bumps, `version_hash` deterministic); `version_hash` change-detection (config edit bumps it, plain restart does not); `restart ≠ recreate`; identity-mandatory fail-closed paths; elevated-scope gate returns the typed sentinel.
- **Integration:** `test/integration/agent_registry_test.go` — StateStore-backed rehydration exercised across **all three** StateStore drivers (in-mem / SQLite / Postgres) with real drivers on the seam; real `events.EventBus` (inmem driver) asserting `agent.*` emission; identity propagation through every layer; ≥1 failure mode (missing identity fails closed; a forced StateStore error surfaces, not silently swallowed).
- **Conformance:** `internal/runtime/registry/conformance` — cross-tenant / cross-session isolation: N tenants × M sessions, every registration resolves only for its owning scope.
- **Concurrency / leak:** `d025_test.go` — N≥100 concurrent `Register`/`Get`/`List`/control calls against one shared registry under `-race`; per-goroutine identity round-trip (no context bleed); pre-cancelled ctx on a subset (no cross-talk); baseline goroutine count restored after teardown.

## Smoke script additions

- `scripts/smoke/phase-53a.sh` documents that the Agent Registry is a runtime-internal subsystem at Phase 53a with **no Protocol surface** — `agents.*` Protocol methods land in the Protocol / Console-attaching waves and twin with their own smoke coverage. The script therefore `skip`s (per the §4.2 404/405/501 → SKIP convention that lets phase-N+1 surfaces coexist with phase-N builds). When a later phase exposes the registry over the Protocol, that phase's smoke script asserts the live surface.

## Coverage target

- `internal/runtime/registry`: 85%
- `internal/runtime/registry/conformance`: 85%

## Dependencies

- 01 (identity), 05 (events — `agent.*` payloads ride the typed bus), 07 (StateStore iface + InMem — persistence), 08 (SessionRegistry — agents register within a session scope).

Note: dependencies are all long-shipped. Phase 53a is slotted into the 50–53 band per the master-plan planning decision (earlier runtime-subsystem bands are already shipped); it is parallelizable with the 50→54 pause/resume + steering chain and must land before Phase 54 and the Console-attaching wave (72–75) that consume it.

## Risks / open questions

- **`version_hash` input surface.** The hash must be deterministic across runs and stable under semantically-irrelevant reordering (e.g. tool registration order). Mitigation: canonicalise the input (sorted tool list by name, sorted schema keys) before hashing; `version_test.go` pins the determinism.
- **Restart rehydration with a non-durable StateStore.** The in-mem driver loses the registry on restart. This is documented dev-only behaviour, surfaced explicitly (not a silent empty registry that looks like data loss). Operators running a fleet use SQLite/Postgres.
- **Elevated-scope gate shape.** The exact scope-claim representation for fleet control is pinned to the Protocol auth surface (Phase 61). At this phase the gate is a typed seam (`ErrElevatedScopeRequired`); Phase 61 wires the concrete claim check. This is the §13 "primitive with its consumer" boundary — the gate ships with a unit test exercising it, and the Protocol consumer twins later.
- **Remote-agent liveness.** A connect-to-remote agent's health depends on the A2A peer being reachable. V1 stores the handle + AgentCard reference; health polling of remote peers is a later concern.

## Glossary additions

- `Agent Registry`, `agent_id`, `incarnation`, `version_hash`, `Fleet control / fleet observation` — all added to `docs/glossary.md` in this PR's batch.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. — `internal/runtime/registry/d025_test.go`.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md §17. — `test/integration/agent_registry_test.go` (consumes StateStore + EventBus).
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — brief 09 §170, justified above, D-059.
