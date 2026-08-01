# Phase 228 — Prepared MCP activation

## Summary

Closes #653 by replacing live-first add-connection compensation with prepare, persist, then activate. An unpublished preparation can validate a real server but cannot replace or expose a live tool before desired state is durable.

## RFC anchor

- RFC §3.3.
- RFC §6.4.
- RFC §6.11.
- RFC §6.16.

## Briefs informing this phase

- brief 14
- brief 09
- brief 03

## Brief findings incorporated

- brief 14 §1: the official MCP SDK owns protocol mechanics; Harbor owns honest publication and lifecycle.
- brief 09: OAuth acquisition uses the unified pause/resume primitive and scoped provider ownership.
- brief 03 §5: transport lifecycle and credentials fail loudly through one tool-provider path.

## Findings I'm departing from (if any)

- D-381's unreadable-pointer fallback may detach a landed connection. This phase supersedes that compensation direction with unpublished fail-closed preparation.

## Goals

- Make add-connection publication transactional with desired-state persistence and preserve an existing healthy registration on refusal.

## Non-goals

- No live-registry re-key, no `agent_id` isolation axis, and no ScopeUser OAuth re-key (#638 remains deferred).

## Acceptance criteria

- [x] Prepare may dial/handshake/discover but cannot publish tools, providers, or `online` state.
- [x] Persisted desired state precedes activation; the registry reserves reversibly and privately before the catalog swap linearizes dispatch, and same-owner replacement preserves the old callable registration and direct registry reads until that point.
- [x] Pointer landed/not-landed/unreadable outcomes converge without unconditional detach; an exactly landed auth-required write preserves and returns its producer pause token even when the write acknowledgement is lost.
- [x] Inline OAuth rollback uses an exact installation receipt and never removes another call's provider.
- [x] Auth-required preparation parks through unified pause/resume and is restart-safe, including sealed PKCE/state/client/pause correlation, atomic credential persistence, a sealed exact-state completion tombstone retained through a bounded retry horizon, exact landed/partial cleanup convergence, identity-scoped expiry pruning, contained denial classification, durable terminal-rejection retry, and first-winner mixed resume decisions.
- [x] Run-start reconcile uses the same lifecycle and descriptor equality.
- [x] Real spec-derived MCP fixture, failure injection, cancellation, N≥100 reuse, and leak tests pass.

## Files added or changed

- `internal/runtime/agentcfg/protocol/`, `internal/runtime/serve/`
- `internal/tools/drivers/mcp/`, `internal/tools/auth/`, auth provider wiring
- `scripts/smoke/phase-228.sh`

## Public API surface

- Internal mandatory `ConnectionPreparer`, single-use `PreparedConnection`, and sealed `FlowStore` seams; no Protocol wire change.

## Test plan

- **Unit:** state table, activation receipt, replacement, rollback, mixed resume arbitration, sealed flow claims, exact callback idempotency after credential replacement and acknowledgement loss, partial-cleanup convergence, identity-scoped tombstone expiry, denial containment across logs/responses/pause/events, terminal retry, and single-record token persistence.
- **Integration:** real MCP fixture through Protocol, agentcfg, registry, catalog, OAuth callback, provider reconstruction, and durable pause reconstruction.
- **Conformance:** prepare has zero publication; activation is all-or-nothing.
- **Concurrency / leak:** same-name and cross-owner stress, cancellation and teardown.

## Smoke script additions

- Run the no-prepublication, unreadable-pointer, replacement, reconciliation, durable OAuth restart, exact completion-tombstone, partial-cleanup, expiry-pruning, claim, cancellation, and mixed-decision tests.

## Coverage target

- Touched agentcfg/serve/MCP packages do not fall below v1.25 floors.

## Dependencies

- 226, 198, 203.

## Risks / open questions

- #638 must land before D-379's cross-owner same-name gates can ever be relaxed.

## Glossary additions

- Prepared connection.

## Pre-merge checklist

- [ ] Drift, mirror, CI preflight, spec-fixture integration, identity, concurrent-reuse, and leak gates pass
