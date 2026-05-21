# Phase 85a — mcp-client-core-compliance

## Summary

Close the two correctness defects and two missing notification handlers that stand between Harbor and a clean, honest "MCP 2025-11-25 core-compliant client" claim. Fix the `Discover` pagination-truncation bug, stop advertising the `roots.listChanged` capability Harbor cannot service, add `ToolListChanged` / `ResourceListChanged` / `PromptListChanged` handlers so the catalog refreshes mid-session, and add resource `Unsubscribe` so subscriptions don't leak on Close. This is the **foundation phase** of the 85-band: 85e replaces the roots stopgap with a real provider; 85b–g/h–j build outward.

## RFC anchor

- RFC §6.4

## Briefs informing this phase

- brief 14
- brief 03

## Brief findings incorporated

- brief 14 §3: "`Discover` reads page 1 only; `NextCursor` ignored. A server exposing >1 page of tools silently loses every tool past page 1 — a *silent correctness* failure, the worst kind." — this phase loops the cursor (the SDK's auto-paginating iterators are the mechanism).
- brief 14 §3: "Harbor advertises `roots.listChanged` … but wires no provider … this disqualifies Harbor from a clean 'core compliant' claim." — this phase advertises an explicit empty `Capabilities` (no roots) until Phase 85e ships the real provider.
- brief 14 §2 (#17, #22): "no `ToolListChangedHandler` — catalog never refreshes" and "No `Unsubscribe` — subscription leak on Close." — both closed here.
- brief 14 §4: "Harbor is not cleanly 'core compliant' today … blocked by the two §3 defects." — the phase's done-definition is exactly "clean core-compliant claim is now true."

## Findings I'm departing from (if any)

- None. This phase is bug-closure on shipped Phase 28 code; it adds no new design surface.

## Goals

- `Discover` returns the *complete* tool / resource / prompt catalog regardless of server-side pagination.
- Harbor advertises only capabilities it services — the `roots.listChanged` advertisement is removed (re-enabled honestly by 85e).
- The Harbor tool catalog reflects mid-session server changes: a server adding/removing a tool triggers a catalog refresh.
- Resource subscriptions are released on provider Close — no leaked `resources/subscribe` registrations.

## Non-goals

- A real roots provider — Phase 85e. This phase only stops the dishonest advertisement.
- OAuth, sampling, elicitation, completions, logging, resource templates, progress, Apps, Tasks — later phases in the band.
- Any change to content lowering (`content.go`) — it is already correct.

## Acceptance criteria

- [ ] `Discover` follows `NextCursor` for `tools/list`, `resources/list`, and `prompts/list` until exhausted — verified with a mock server returning a 3-page tool list; all pages' tools appear in the resulting catalog.
- [ ] `ClientOptions.Capabilities` is set explicitly to a value that does **not** advertise `roots.listChanged`. A test connects to a mock server and asserts the negotiated client capabilities omit `roots`.
- [ ] `ToolListChangedHandler`, `ResourceListChangedHandler`, `PromptListChangedHandler` are registered; receiving `notifications/tools/list_changed` (etc.) triggers a re-`Discover` and updates the Harbor catalog. A `mcp.catalog_refreshed` event is emitted (identity-stamped).
- [ ] `Unsubscribe` is called for every active resource subscription on `Provider.Close`; a test asserts no subscription survives Close (mock server records subscribe/unsubscribe pairs).
- [ ] Concurrent-reuse: the provider remains D-025-safe — N≥100 concurrent `Invoke` calls during a list-changed refresh pass under `-race` with no catalog corruption.
- [ ] All Phase 28 tests still pass; no regression in stdio / streamable / SSE transport behaviour.

## Files added or changed

- `internal/tools/drivers/mcp/mcp.go` — cursor loop in `Discover`; explicit `Capabilities`; `*ListChanged` handler registration.
- `internal/tools/drivers/mcp/events.go` — `mcp.catalog_refreshed` event type + emit.
- `internal/tools/drivers/mcp/registry.go` — track active subscriptions for Close-time `Unsubscribe`.
- `internal/tools/drivers/mcp/mcp_test.go`, `mockserver_test.go` — multi-page list fixtures; list-changed + unsubscribe assertions.
- `internal/events/` taxonomy — register `mcp.catalog_refreshed` if not already present.
- `scripts/smoke/phase-85a.sh` — static-only assertions.
- `docs/glossary.md` — add `MCP capability negotiation`, `MCP Tasks`, `Task-augmented request`, `MCP Apps`, `Related-task metadata` (band-wide glossary terms land with the foundation phase per brief 14 §9).
- `docs/plans/README.md` — Status column flip on merge.

## Public API surface

No new exported types. `Provider.Discover` behaviour changes (now exhaustive); `Provider.Close` behaviour changes (now releases subscriptions). Both are bug fixes, not API changes — callers are unaffected.

## Test plan

- **Unit:** cursor-loop termination (1-page, 3-page, empty); capability assertion; `*ListChanged` → refresh; Close → `Unsubscribe` for N active subscriptions.
- **Integration:** in-process mock MCP server (`mockserver_test.go`) exercising a multi-page catalog + a mid-session `tools/list_changed` + a resource subscription across Close. Real driver, identity propagation, `-race`.
- **Conformance:** N/A — the band's conformance harness is Phase 85j.
- **Concurrency / leak:** N≥100 concurrent invokes during a refresh; goroutine-leak baseline restored after Close.

## Smoke script additions

- `scripts/smoke/phase-85a.sh` (classification: `static-only`):
  - Assert `internal/tools/drivers/mcp/mcp.go` no longer calls `ListTools` with a discarded result (grep for the cursor-loop helper name).
  - Assert the events taxonomy contains `mcp.catalog_refreshed`.

## Coverage target

- `internal/tools/drivers/mcp`: 85% (Phase 28's conformance-tested target; this phase must not drop it).

## Dependencies

- 28 (MCP southbound driver — the code this phase fixes).

## Risks / open questions

- **SDK iterator semantics.** The go-sdk's auto-paginating iterators may have their own error/termination behaviour; the cursor loop must surface partial-page errors loudly, not swallow them. A forced mid-pagination error is a test case.
- **List-changed storm.** A misbehaving server could spam `list_changed`; the refresh must be debounced (coalesce rapid notifications into one `Discover`) to avoid a refresh loop. Debounce window is a documented constant.

## Glossary additions

See brief 14 §9 — the five band-wide terms (`MCP capability negotiation`, `MCP Tasks`, `Task-augmented request`, `MCP Apps`, `Related-task metadata`) land in `docs/glossary.md` with this phase as the band's foundation PR.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] Coverage ≥ target
- [ ] Cross-isolation test — N/A (catalog refresh is identity-stamped; no cross-identity surface added).
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent invokes during a refresh under `-race`.
- [ ] Integration test passes — mock MCP server, real driver, multi-page + list-changed + unsubscribe.
- [ ] Glossary updated.
- [ ] No brief departures.
