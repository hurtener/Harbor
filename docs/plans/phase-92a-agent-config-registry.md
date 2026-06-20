# Phase 92a — agent-config control plane: versioned desired-state registry + Protocol diff/rollback

## Summary

Builds the unifying primitive of the agent-config control plane: a durable, identity-scoped, **versioned desired-state registry** on the StateStore where every edit to an agent's configuration (prompt layers, tool/MCP exposure, per-tool policy, skills) is an immutable, content-addressed revision, the active config is a pointer to a revision, **rollback is a repoint**, and **diff is a server-side revision compare**. Ships the admin-scoped Protocol surface (`agent_config.*`: get / set_revision / list_revisions / diff / rollback) plus the `agent.config.revised` / `agent.config.reverted` canonical events. This phase delivers mechanism only; its first consumer (skills control, 92c) lands in the same wave per CLAUDE.md §13.

## RFC anchor

- RFC §6.16 — Agent Registry (the `version_hash` content-hash over prompt set + tool set + planner config + model policy this registry makes editable + revisioned; `agent_id` as registration identity, not isolation principal).
- RFC §6.15 — Governance subsystem (the desired-state-over-Protocol + next-turn-reconcile pattern this generalises from LLM governance into agent-definition config).
- RFC §7.4 — Out of scope V1 (the unified pause/resume primitive the later add-connection path reuses; not built here).

## Briefs informing this phase

- brief 11
- brief 05
- brief 09

## Brief findings incorporated

- **brief 11 (console feature surface):** the Agents page is a *fleet-management lens* over the Agent Registry, not an assistant gallery — config history + diff + rollback are operator surfaces rendered from canonical events + a registry snapshot, never a Console-held config store. This phase emits `agent.config.revised`/`reverted` and exposes a read surface so the Console stays a Protocol client (D-061).
- **brief 05 (state / tasks / artifacts / sessions):** immutable, content-addressed records with parent pointers are the durable-revision idiom; the StateStore `Kind`-prefixed record + the isolation triple give versioning identity isolation for free. This phase stores parent pointer + content hash INSIDE the record bytes (never relying on the evictable `EventID` slot) and enumerates revisions via `ListKind` under the elevated maintenance scope.
- **brief 09 (mcp oauth / agent-as-actor):** an agent is a runtime entity with a registration identity (`agent_id`); tokens and config bind to that identity, but it never widens the isolation boundary. This phase keys the registry by `agent_id` while scoping every StateStore record by the `(tenant, user, session)` triple — `agent_id` is never a `WHERE`-clause isolation filter.

## Findings I'm departing from (if any)

None.

## Goals

- A new `internal/agentcfg` subsystem (behind the §4.4 seam: interface + StateStore-backed driver + factory/registry) that stores immutable, content-addressed config revisions and an active-revision pointer, keyed by `agent_id` + identity triple.
- Revision model: `{revision_id, parent_revision_id, content_hash, author, created_at, payload}`; the payload is a typed, forward-compatible envelope with optional sections (prompt layers, tool/MCP exposure, per-tool policy, skills-set) so later consumers (92c/92d) extend it without a schema break.
- Admin-scoped Protocol surface: `agent_config.get`, `agent_config.set_revision`, `agent_config.list_revisions`, `agent_config.diff`, `agent_config.rollback` on the `/v1/agent_config/` family, gated per D-235.
- Server-side diff: text diff for prompt sections, structured set-diff for the structured sections.
- Canonical events `agent.config.revised` + `agent.config.reverted` carrying agent_id + revision ids + author identity (never payload secrets).
- The run-start projection seam: a `RunContext`-resolved active-config view read ONCE at run start (extends `tools.NewPlannerView` / the 92b run-start LLM resolution), so application is next-turn-only and D-025-aligned. (This phase wires the seam + a no-op/identity projection; 92c/92d attach real domain projections.)

## Non-goals

- Per-domain semantics (skills control = 92c; MCP pause/per-tool policy = 92d; layered prompt = 92e; add-new-connection = 92f; session-user safe subset = 92g). This phase ships the registry, the Protocol surface, diff/rollback, the events, and the run-start projection seam — with skills (92c) as its same-wave consumer.
- Mid-flight reconciliation / per-run hot-swap (forbidden by D-025; next-turn only).
- Adding a genuinely new MCP connection (async dial + handshake + OAuth) — 92f.
- Automatic revision GC / compaction (documented manual `ListKind`+prune for V1).
- The session-user safe subset and its gating (D-235 reserves it for 92g).

## Acceptance criteria

- [ ] `internal/agentcfg` defines the `Registry` interface + a StateStore-backed driver + factory/registry dispatching by name; the driver self-registers and is pulled into `internal/drivers/prod` (§4.4).
- [ ] Each `SetRevision` writes a NEW immutable revision (content-addressed full 32-byte SHA-256; parent pointer + hash stored in the record bytes) and advances the active pointer; an idempotent re-set of identical content is a no-op that returns the existing revision id.
- [ ] `Rollback` writes a new active pointer to an existing revision id WITHOUT mutating or deleting any revision; rolling back to a non-existent revision fails loud with a typed error.
- [ ] `Diff(a, b)` returns a text diff for prompt sections + a structured set-diff for the structured sections; diffing across two existing revisions is deterministic.
- [ ] `ListRevisions` returns the revision chain newest-first under the elevated maintenance scope; cross-identity enumeration without the scope fails closed.
- [ ] The five `agent_config.*` Protocol methods exist (single-source: methods.go + singlesource), wire types in `internal/protocol/types/agentconfig.go`, route `POST /v1/agent_config/{method}`, gated on the verified admin scope (D-235), nil-safe to 501 when the subsystem is not wired.
- [ ] `agent.config.revised` + `agent.config.reverted` canonical events are declared, emitted on success, redacted, and carry no payload secrets.
- [ ] The run-start projection seam reads the active revision ONCE per run into the per-run snapshot; concurrent/in-flight runs keep their snapshot (D-025).
- [ ] Identity: every registry record is scoped by the `(tenant, user, session)` triple; `agent_id` is a key field, never an isolation filter (§6).
- [ ] TS wire manifest regenerated + the per-page `agent_config.ts` typed module (or a justified allow-list entry); generated Protocol docs regenerated; `scripts/smoke/phase-92a.sh` green.

## Files added or changed

- `internal/agentcfg/agentcfg.go` — the `Registry` interface + factory/registry + revision/payload types + content-hash + diff helpers.
- `internal/agentcfg/drivers/statestore/` — the StateStore-backed driver (one driver per subdir, §4.4).
- `internal/agentcfg/events.go` — `agent.config.revised` / `agent.config.reverted` event types + payloads.
- `internal/agentcfg/protocol/` — the `agent_config.*` service (validate identity, drive the registry, emit events; mirrors `governance/protocol`).
- `internal/protocol/methods/methods.go` — five `MethodAgentConfig*` constants + `canonicalAgentConfigMethods` + `IsAgentConfigMethod` / `IsAgentConfigControlMethod` + `IsControlMethod` exclusion.
- `internal/protocol/types/agentconfig.go` — wire request/response types (single source).
- `internal/protocol/singlesource/singlesource.go` — `CanonicalMethods` + `CanonicalWireTypes` entries.
- `internal/protocol/transports/stream/agentconfig_handler.go` + `transports.go` mount (`POST /v1/agent_config/`).
- `cmd/harbor-protocol-ts-lockstep/manifest.go` — `typeInstanceIndex` entries.
- `web/console/src/lib/protocol/agentconfig.ts` + `client.ts` namespace + `harbor.ts` re-export; `wire-manifest.gen.json` regenerated.
- `cmd/harbor/cmd_dev.go` + `cmd_dev_runloop.go` — wire the registry; resolve the active-config view at run start (the projection seam); **the `harbortest/devstack` twin mirrors it in the same PR (D-094)**.
- `internal/drivers/prod/` — blank-import the StateStore driver.
- `docs/site/protocol/*` — regenerated; `docs/skills/use-the-harbor-protocol/SKILL.md` — note the new admin family.
- `scripts/smoke/phase-92a.sh`.

## Public API surface

```go
package agentcfg

// Registry is the durable, identity-scoped, versioned desired-state store.
// Compiled artifact: immutable after construction; safe for concurrent reuse (D-025).
type Registry interface {
    SetRevision(ctx context.Context, id identity.Quadruple, agentID string, payload ConfigPayload) (Revision, error)
    Active(ctx context.Context, id identity.Quadruple, agentID string) (Revision, bool, error)
    Get(ctx context.Context, id identity.Quadruple, agentID, revisionID string) (Revision, error)
    ListRevisions(ctx context.Context, id identity.Quadruple, agentID string, limit int) ([]Revision, error)
    Rollback(ctx context.Context, id identity.Quadruple, agentID, revisionID string) (Revision, error)
    Diff(ctx context.Context, id identity.Quadruple, agentID, fromRev, toRev string) (Diff, error)
    Close(ctx context.Context) error
}

type Revision struct {
    RevisionID, ParentRevisionID, ContentHash string
    Author    identity.Quadruple
    CreatedAt time.Time
    Payload   ConfigPayload
}

// ConfigPayload is the forward-compatible envelope; every section is optional so
// later consumers (92c skills, 92d MCP policy, 92e prompt) extend without a break.
type ConfigPayload struct {
    PromptLayers *PromptLayers       `json:"prompt_layers,omitempty"`
    ToolExposure *ToolExposure       `json:"tool_exposure,omitempty"`
    Skills       *SkillsSelection    `json:"skills,omitempty"`
    // ... extended by 92c/92d
}
```

## Test plan

- **Unit:** revision immutability (a second set never mutates the first); content-addressing (identical payload → same hash → idempotent no-op returning the existing id); parent-pointer chain integrity; rollback never mutates/deletes a revision + fails loud on a missing target; diff determinism (text + structured set-diff); event emission carries ids + author, never payload secrets.
- **Integration:** `test/integration/agentcfg_control_plane_test.go` — real StateStore driver + real event bus + the real `agent_config` service end-to-end: set → list → diff → rollback round-trip; `agent.config.revised`/`reverted` observed on the bus with identity propagated; the run-start projection reads the active revision into a per-run snapshot; ≥1 failure mode (rollback to missing revision; non-admin rejected with `CodeScopeMismatch`); under `-race`.
- **Conformance:** the StateStore-backed driver passes a small `agentcfg` driver conformance suite (set/active/get/list/rollback/diff parity), so future drivers inherit it.
- **Concurrency / leak:** N≥100 concurrent `SetRevision` + `Active` + `Diff` against ONE shared `Registry` under `-race` — no data races, no context bleed (run A's revision never reaches run B), no cross-cancellation, baseline goroutines restored (D-025).

## Smoke script additions

- `scripts/smoke/phase-92a.sh`: static — the five `agent_config.*` method constants + the `canonicalAgentConfigMethods` set + `internal/agentcfg` package + the two canonical events + the typed `agentconfig.ts` module + the generated-docs join rows; live (skip-if-404) — `agent_config.set_revision` then `agent_config.get` round-trips through the admin-gated route, a non-admin token is rejected, `agent_config.diff` of two revisions returns a diff.

## Coverage target

- `internal/agentcfg`: 85%
- `internal/agentcfg/protocol`: 85%
- `internal/agentcfg/drivers/statestore`: 85%

## Dependencies

- 92, 92b (the governance desired-state + next-turn pattern + run-start resolution seam), 87 + 86 (durable tasks + bus — mid-session config + reconnect coherence), 53a (agent registry / `agent_id`), 110a (`NewPlannerView` projection seam). Plus, for the first wave, the same-wave consumer 92c (skills).

## Risks / open questions

- **Revision bloat (no GC).** The StateStore has no TTL; high-frequency edits grow the store unbounded. V1 answer: a documented manual `ListKind`+prune path; automatic compaction is a recorded follow-up. The smoke/docs call this out so it is not a silent cap.
- **Payload schema evolution.** The `ConfigPayload` envelope must stay forward-compatible across 92c/92d/92e; every section is an optional pointer and the content hash is computed over a canonical (sorted-key) encoding so a section addition does not spuriously change unrelated revisions' hashes.
- **`agent_id` resolution in `harbor dev`.** The dev run loop runs a single configured agent; the registry key is that agent's registration id. Multi-agent fleets exercise it more fully — flagged for the integration test.

## Glossary additions

- **agent-config control plane** — the Protocol-driven, durable, versioned surface for live-controlling an agent's configuration (prompt layers, tool/MCP exposure, per-tool policy, skills) with diff + rollback, applied next-turn.
- **desired-state registry** — the durable, identity-scoped, versioned StateStore-backed store of agent-config revisions; the unifying primitive of the control plane.
- **config revision** — an immutable, content-addressed agent-config record with a parent pointer; the active config is a pointer to one revision; rollback is a repoint.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **Concurrent-reuse test passes (N≥100 against one `Registry` under `-race`).** The registry is a reusable compiled artifact.
- [ ] **Integration test exists (`test/integration/agentcfg_control_plane_test.go`), real StateStore + bus + service, identity propagation, ≥1 failure mode, `-race`.** Deps names shipped phases.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
