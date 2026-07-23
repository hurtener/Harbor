# Phase 198 — Live-layer idempotent MCP re-attach (HA-33)

## Summary

`agent_config.add_mcp_connection` becomes idempotent at the live layer: a same-name attach whose name already has a live in-memory registration synchronously REPLACES it — deregister the old server's catalog tools + close its transport, then register the new connection — instead of blind-registering and failing `catalog.Register` with a duplicate-tool-name collision. This closes the synchronous-attach / deferred-detach asymmetry (issue #375) that otherwise strands a re-attaching coordinator with zero tools until a process restart, and fixes the compounding `Registry.Register`-overwrite transport leak in the same change.

## RFC anchor

- RFC §6.4

## Briefs informing this phase

- brief 03
- brief 14

## Brief findings incorporated

- brief 03 §"Tool catalog + transports": every `Tool` is the same struct regardless of source and dispatch is one switch — so a same-name replace is a catalog/registry operation, not a transport-specific special case; the replace reuses the generic `DeregisterSource` + `Registry.Deregister` legs rather than inventing an MCP-only teardown.
- brief 14 §"MCP client/host compliance": a connection's transport (stdio / streamable-HTTP / SSE) owns a live session that must be explicitly closed on teardown — a bare map overwrite that drops the `*serverEntry` without `provider.Close(ctx)` leaks the session; the replace MUST route through the transport-closing deregister leg.

## Findings I'm departing from (if any)

None.

## Goals

- A same-name `add_mcp_connection` against a still-live registration succeeds and leaves the NEW tool set live, with the OLD transport closed.
- The replace is atomic and race-free under the attacher's existing serialise-the-whole-attach lock — no new lock, no deadlock.
- The compounding `Registry.Register` same-name overwrite (which leaks the prior transport) is fixed to route through the transport-closing deregister.
- A first attach with no prior same-name registration is unaffected (the deregister legs no-op).

## Non-goals

- Moving the general detach earlier / making `remove_mcp_connection` synchronous — the deferred run-start reconcile (#375) remains the authority for a *removed* connection. This phase only makes RE-ATTACH self-healing.
- Any new Protocol verb (`teardown_now` / forced-reconcile) — explicitly rejected in D-339.
- Any wire-type change — the request/response shapes are unchanged (idempotency is a server-side behavior change).

## Acceptance criteria

- [ ] `add_mcp_connection` for a name with a live same-name registration deregisters the old server's catalog tools (`DeregisterSource`) AND closes its transport (`Registry.Deregister`) BEFORE registering the new connection, returning `state: online` with the new tools live.
- [ ] The replace happens inside the attacher's existing attach mutex — no new lock introduced; a concurrent-reuse test (N≥100 interleaved same-name attach/re-attach against one shared attacher, `-race`) shows no duplicate-registration error, no leaked transport, no cross-talk.
- [ ] `Registry.Register`'s same-name path no longer silently overwrites: same-name replacement closes the prior provider's transport (goroutine-baseline test asserts no leak after a replace).
- [ ] A first attach (no prior registration) is behaviorally identical to today (deregister legs no-op via the `ErrServerNotFound` swallow).
- [ ] `scripts/smoke/phase-198.sh` asserts a live add → same-name re-add returns success (or SKIPs cleanly where no dev MCP fixture is reachable).

## Files added or changed

```text
internal/tools/drivers/mcp/attach.go              # same-name replace before the register loop (deregister source + registry deregister, then Connect/Discover/Register)
internal/tools/drivers/mcp/registry.go            # Register same-name path routes through Deregister (transport close) instead of a bare map assignment
internal/tools/drivers/mcp/attach_reattach_test.go # NEW — re-attach replace + first-attach-unaffected + concurrent-reuse (N≥100, -race) + transport-leak
internal/runtime/serve/mcp_attacher_test.go        # re-attach path exercised through the production attacher (no new lock)
scripts/smoke/phase-198.sh                         # NEW — live add → same-name re-add → success (degrades to SKIP)
docs/plans/phase-198-mcp-reattach-idempotency.md   # this plan
docs/glossary.md                                   # "Live-layer MCP re-attach (idempotent replace)"
```

## Public API surface

No new exported API. The behavioral contract of the existing attach path changes: a same-name attach is now an atomic upsert. (`tools.CatalogSourceDeregisterer.DeregisterSource` and `mcpdrv` `Registry.Deregister` already exist and are reused.)

## Test plan

- **Unit:** re-attach with a live same-name registration → new tools live, old transport closed; first attach (no prior) unaffected; `Registry.Register` same-name replace closes the old transport; deregister-legs no-op on unknown name.
- **Integration:** through `internal/runtime/serve` production attacher — add → re-add same name → assert catalog reflects the new tool set and the old transport is closed; identity/agent-registration propagation intact.
- **Conformance:** N/A — single MCP driver behavior, no multi-driver interface added.
- **Concurrency / leak:** N≥100 interleaved same-name attach/re-attach against one shared attacher under `-race` (D-025) — no duplicate-registration, no cross-talk; goroutine-baseline returns after teardown (no leaked transports).

## Smoke script additions

- `scripts/smoke/phase-198.sh` (`PREFLIGHT_REQUIRES: live-server`): assert `agent_config.add_mcp_connection` is a known method (present in `wire-manifest.gen.json`); when a dev MCP fixture is reachable, add a connection then re-add the SAME name and assert the second call returns a success state (not a duplicate-tool-name failure); SKIP cleanly on 404/405/501 or when no fixture is configured.

## Coverage target

- `internal/tools/drivers/mcp`: ≥ 80% on the touched attach/registry paths.

## Dependencies

- Gate-0 (D-339). Builds on the shipped runtime MCP attach lifecycle (92k–92q / #375) — all already on `dev-experimental`.

## Risks / open questions

- The replace runs two teardown legs (catalog + registry) inside the attach mutex before the new Connect; if the new Connect then fails, the old tools are already gone. This is acceptable and intended (a re-attach the caller asked for supersedes the old registration) — the attach returns `failed` with the old tools removed, matching the "operator asked to replace" intent; documented in the plan and asserted by test. No RFC §11 open question.

## Glossary additions

- **Live-layer MCP re-attach (idempotent replace)** — a same-name `add_mcp_connection` against a still-live registration that atomically deregisters the old server's tools + closes its transport, then registers the new connection. D-339.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A (attach is admin-gated control-plane keyed to the agent registration, not an isolation-tuple path).
- [ ] **Concurrent-reuse test passes** — N≥100 interleaved same-name attach/re-attach against a single shared attacker under `-race` (the attacher is a reusable artifact). See §5 + D-025.
- [ ] **Integration test exists** — the re-attach path exercised through the production `internal/runtime/serve` attacher end-to-end (Deps names shipped MCP-attach phases).
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — N/A (no departure).
