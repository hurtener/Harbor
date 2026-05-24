# Phase 83w — wire-surface-gaps

## Summary

Closes Bugs **F5** + **F6** from the round-2 walkthrough — two
wire-surface gaps that surface as scary red ERROR PageStates on the
operator's primary debugging surfaces.

- **F5.** Live Runtime + Playground both call `topology.snapshot`
  unconditionally. The dev binary's runtime is planner/RunLoop-shaped
  (no engine graph); the method returns `unknown_method`. The
  PageState's `error` branch displays "Request failed · unknown_method"
  + Retry button on EVERY visit. Operators see red on the most-used
  pages.
- **F6.** MCP Connections page calls `mcp.servers.list` which
  returns `unknown_method`. The MCP registry IS attached (Phase 83g)
  and the Tools page shows 6 youtube tools fine — the wire surface
  just doesn't expose the registry's list method.

Two distinct fixes; one Go side (F6), one Console side (F5).

## RFC anchor

- RFC §5 — Console as Protocol client.
- RFC §6.4 — Tool catalog + MCP southbound (F6).
- RFC §7 — Console feature surface (F5).

## Briefs informing this phase

- brief 11
- brief 14

## Brief findings incorporated

- brief 11 §3 — Empty / "not available" is a first-class state, not
  an error. F5's "unknown_method" on a not-applicable surface
  shouldn't surface as a red error.
- brief 14 §3 — The MCP registry is reachable from the
  ControlSurface; an `mcp.servers.list` method that returns the
  attached servers is mechanical.

## Findings I'm departing from (if any)

None.

## Goals

### F5 — friendly `unknown_method` for topology.snapshot

- The Console-side error handler for `topology.snapshot` (in the
  Live Runtime page + Playground page) special-cases the
  `unknown_method` error code → render an "info" branch ("Topology
  view not available on this Runtime — this is a planner/RunLoop
  runtime, not an engine-graph runtime; see docs/CONFIG.md for the
  runtime shape") instead of the scary red error.
- Reusable shape: the `PageState` component gains an `info` branch
  (additive to disconnected / loading / error / empty / ready),
  OR the per-page handler maps the error to the empty state with
  a custom message.

### F6 — Runtime exposes `mcp.servers.list`

- Add `mcp.servers.list` to the Runtime's wire surface
  (`internal/runtime/registry/protocol/` or `internal/protocol/mcp.go`).
  The handler reads the `*mcp.Registry` already constructed at
  `bootDevStack` line 657 and returns the list of attached servers
  in the established wire shape (`prototypes.MCPServerRow`).
- The page already knows how to render the response (TOOL OVERVIEW
  card, status chip column, etc.); just need the handler.

## Non-goals

- **Polish on the MCP Connections page** when zero servers attached.
  Out of scope; the empty state already exists.
- **Streaming MCP updates** (`mcp.servers.subscribe` or similar) —
  V1.1 wires the list call only; subscribe is post-V1.

## Acceptance criteria

### F5 (Console side)

- [ ] The Live Runtime page's topology-snapshot call routes through
      a special-case handler: on `unknown_method`, render a
      friendly "Topology view not available on this Runtime" info
      banner with a docs link.
- [ ] Same for the Playground page.
- [ ] Existing topology-snapshot tests still pass.
- [ ] The fix is small + localised (probably 30-50 LOC in the two
      page files, OR a shared error-mapping helper).

### F6 (Go side)

- [ ] `mcp.servers.list` method handler lands at
      `internal/protocol/mcp.go` (or wherever the existing MCP
      wire surface lives — F6 may extend, not introduce, this
      package).
- [ ] The handler reads the `*mcp.Registry` (constructed in
      `bootDevStack`) and projects to the wire shape.
- [ ] Wired into the ControlSurface dispatch.
- [ ] Test covering the happy path (one attached server) + the
      empty path (zero attached).
- [ ] MCP Connections page now renders the youtube server row
      with its 6 tools, online status, etc.

### Cross-bucket

- [ ] `scripts/smoke/phase-83w.sh` asserts both surfaces.

## Files added or changed

### F5 (Console side)

- `web/console/src/routes/(console)/live-runtime/+page.svelte` —
  unknown_method special case.
- `web/console/src/routes/(console)/playground/+page.svelte` (or
  its detail page) — same.
- Maybe a small helper in `web/console/src/lib/protocol/errors.ts`
  to encapsulate the special-case.

### F6 (Go side)

- `internal/protocol/mcp.go` — `mcp.servers.list` handler.
- `internal/runtime/registry/protocol/...` (if the handler lives
  there) — wire-up.
- `cmd/harbor/cmd_dev.go` — pass the `mcpRegistry` into the
  ControlSurface where the handler reads it from.
- `harbortest/devstack/devstack.go` — D-094 mirror.
- Tests at `internal/protocol/mcp_test.go` (or new file).

### Coordinator

- `docs/plans/README.md` — Phase 83w row.
- `docs/decisions.md` — D-164.
- `docs/plans/phase-83w-wire-surface-gaps.md` — this plan.
- `scripts/smoke/phase-83w.sh`.

## Public API surface

- New Protocol method `mcp.servers.list`. Wire shape uses existing
  `prototypes.MCPServerRow` + `prototypes.MCPServersListResponse`
  (or whatever the existing surface defines — confirm in
  `internal/protocol/types/mcp_servers.go`).

## Test plan

- **Unit:** F5 — the special-case error mapper; F6 — handler
  unit test against a stub registry.
- **Integration:** F6 — a Playwright test that boots devstack
  with one mcp.servers entry + asserts the page shows the row.
- **Manual:** run the round-2 walkthrough's Live Runtime +
  Playground + MCP Connections pages; all three render correctly
  with no red errors.

## Smoke script additions

- F5 — Live Runtime + Playground source contains the
  unknown_method special case.
- F6 — `internal/protocol/mcp.go` has the `mcp.servers.list`
  method registration.

## Coverage target

- Existing.

## Dependencies

- Phase 83g (MCP registry construction).
- Phase 83m item 1 (MCP DefaultIdentity per-push).

## Risks / open questions

- **F5's friendly message duplicates per page.** If F5 needs the
  same copy in 3+ places, factor a small shared component. V1.1
  can ship duplicated; tracking debt is fine.
- **F6's auth scope.** `mcp.servers.list` is read-only;
  identity-required (every Protocol method is). No new scope
  needed.

## Glossary additions

None.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] Coverage on `internal/protocol/mcp.go` updated
- [ ] svelte-check + Playwright e2e pass
- [ ] Concurrent-reuse — F6 handler is stateless; D-025 trivially OK
- [ ] Integration test exists per §17 — yes
- [ ] Glossary updated — N/A
- [ ] If a brief finding was departed from: N/A
