# Phase 108l — console-agents-page

## Summary

Phase 108l ships the Agents page's fleet-control Protocol surface AND rethemes
both Agents routes to the carded, viewport-locked Console-page-polish standard.
The Runtime half adds the five `agents.*` fleet-control methods (pause / drain /
restart / force_stop / deregister) as admin-gated Protocol methods over the
shipped in-process `registry.*` control verbs; the Console half wires those
verbs live on the detail route, wires the previously-placeholder activity feed
to a live `agent.*` `events.subscribe` stream, rebuilds the list + detail routes
as carded viewport-locked pages (the detail as the three-column main canvas the
mock pins, not a right rail), and refactors both ~500-line king files into
controllers + a pure `derive.ts`.

## RFC anchor

- RFC §6.16 — Agent Registry (the `agent_id` registration identity + the
  `agent.*` event taxonomy + fleet control).
- RFC §6.4 — tool catalog and transports (the agent's tool-binding projection).
- RFC §7.2 — Console information architecture (Agents sits under Execution; the
  Console is a Protocol client, never reads Runtime internals).

## Briefs informing this phase

- brief 11
- brief 12

## Brief findings incorporated

- brief 11 §"Agents view": the per-agent surface is identity / planner / tools /
  memory / cost / skills / health + recent activity; the page is a runtime lens
  over the Agent Registry, not a chatbot directory (D-062). The detail is built
  to exactly this surface.
- brief 11 §CC-4: agents are slow-moving catalog data — search / filter is a
  Console-side facet over `agents.list`, not a new server index. The list
  controller keeps facets server-fed via `agents.list` filter + Console-local
  saved views (D-061).
- brief 12 §"Open architectural questions Brief 11 raised, resolved here": an
  agent-management surface is required for the wave; fleet-control verbs are
  control-scope gated (D-066) — "a leaked read-only token must not be able to
  force-stop a fleet." The five verbs are admin-gated end-to-end.

## Findings I'm departing from (if any)

None.

## Goals

- Land the five `agents.*` fleet-control Protocol methods, admin-gated, over the
  shipped `registry.*` control verbs, with honest re-read status.
- Wire the Console detail route's control buttons + activity feed to the live
  surface; flip the D-132/F4 disabled-with-tooltip stub to live.
- Reskin both Agents routes to the carded, viewport-locked page-polish standard;
  build the detail as the three-column main canvas (page-agents.md §4).
- Refactor both king files into controllers + a pure, unit-tested `derive.ts`.

## Non-goals

- Authoring agents in the Console (create / edit) — CLI-side per RFC §7.4.
- A reason-input UI for control verbs (the wire carries `reason`; the V1 UI
  sends an empty reason).
- Versioning / rollback, a permissions ACL editor, a cross-runtime aggregator —
  all post-V1 (page-agents.md §10).

## Acceptance criteria

- [ ] `agents.{pause,drain,restart,force_stop,deregister}` are registered
  through the single-source machinery (methods / singlesource / conformance /
  counts).
- [ ] Each control method is admin-gated: no `admin` claim → `403`
  `identity_scope_required`; identity missing → `401`.
- [ ] Control responses carry the ACTUAL re-read status (pause/restart leave
  status `active`; drain→drained, force_stop→force_stopped,
  deregister→deregistered) — no fabricated state (CLAUDE.md §13).
- [ ] The Console list + detail routes are carded + viewport-locked (no page
  scroll, no horizontal overflow), design-tokens-only, Svelte 5 runes.
- [ ] The detail route is a three-column main canvas (tabbed detail / topology /
  activity+tools+memory), NOT a right rail.
- [ ] Control buttons call the real methods, control-scope gated
  (disabled-with-tooltip without `admin`); the activity feed projects live
  `agent.*` events, honest-empty when quiet.
- [ ] Both king files are refactored into controllers + a pure `derive.ts` with
  a `derive.test.ts` suite.

## Files added or changed

- `internal/protocol/methods/methods.go`, `methods_test.go` — the 5 method
  constants plus the control-method subset and predicate.
- `internal/protocol/types/agents.go` — `AgentControlRequest` /
  `AgentControlResponse`.
- `internal/protocol/singlesource/singlesource.go`,
  `internal/protocol/conformance/conformance.go` — register the 5 methods and
  2 types; bump the counts.
- `internal/runtime/registry/protocol/service.go`, `control_test.go` — the
  `Controller` seam, the 5 control methods, the honest re-read.
- `internal/protocol/transports/stream/agents_handler.go`,
  `agents_handler_test.go` — control gating and routing.
- `cmd/harbor/cmd_dev.go` — wire `WithController`.
- `scripts/smoke/phase-73e.sh` — control-verb live assertions.
- `web/console/src/lib/protocol/client.ts`, `agents.ts` — the typed control
  view.
- `web/console/src/lib/agents/state.svelte.ts`, `detail-state.svelte.ts`,
  `derive.ts`, `derive.test.ts` — the controllers and pure projections.
- `web/console/src/routes/(console)/agents/+page.svelte`,
  `[id]/+page.svelte` — the rebuilt routes.
- `web/console/src/lib/components/agents/ControlButtons.svelte`,
  `DetailHeader.svelte`, `AgentActivityFeed.svelte` — wired live.
- `web/console/tests/agents-page.spec.ts` — the control-scope contract.

## Public API surface

- `agents.pause(id, reason) → AgentControlResponse{agent_id, command, status}`
  and the four sibling verbs (drain / restart / force_stop / deregister), all
  admin-gated.

## Test plan

- **Unit:** `web/console/.../agents/derive.test.ts` (StatusChip mappings,
  display-state derivation, activity projection by payload `AgentID`, honest
  control result copy); Go `control_test.go` (gating, honest re-read,
  sentinels).
- **Integration:** `agents_handler_test.go` wires the real registry controller
  through the handler (identity propagation, control-scope gating, the 404 /
  403 / 401 failure modes), under `-race`.
- **Conformance:** the `TestProtocol_Conformance` descriptor map and count
  assertions cover the 5 new methods.
- **Concurrency / leak:** the registry's existing concurrent-reuse suite covers
  the control path; the handler is stateless per request.

## Smoke script additions

- `scripts/smoke/phase-73e.sh`: each of the 5 control verbs returns admin-gated
  `404` on an unknown id (route mounted and admin-gated) plus a no-identity
  `401`.
- `scripts/smoke/phase-108l.sh`: static-only Console assertions — the two routes
  exist, compose the controllers and `derive.ts`, carry the preserved testids,
  and carry no hand-rolled `fetch`.

## Coverage target

- `internal/runtime/registry/protocol`: 80%.
- `internal/protocol/transports/stream` (agents handler): 80%.
- `web/console/src/lib/agents`: `derive.ts` fully covered by `derive.test.ts`.

## Dependencies

- Phase 53a (Agent Registry — shipped), Phase 73e (Agents read surface —
  shipped), Phase 108b (app-shell chrome — shipped), Phase 108k (the
  page-polish controller pattern — shipped).

## Risks / open questions

- The activity feed is live-only (the runtime has no durable `agent.*`
  read-back) — it is honest-empty until events stream in; this is intended, not
  a gap.
- The plain validation runtime's registry is empty (the synthetic default is
  not a registered row) — populated states are live-verified with
  `HARBOR_DEV_SEED_FIXTURES=1`.

## Glossary additions

None — all terms (`Agent Registry`, `control-scope claim`, `fleet control`)
already in `docs/glossary.md`.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] If this phase builds a reusable artifact: concurrent-reuse test passes. N/A for the Console controllers (per-page browser state, not a shared Go artifact); the registry control path is covered by the registry's existing concurrent-reuse suite.
- [ ] If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists — `agents_handler_test.go` wires the real registry controller through the handler with identity propagation and ≥1 failure mode under `-race`.
- [ ] If new vocabulary: glossary updated — N/A (no new terms).
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — N/A (no departure).
