# Phase 83g — mcp-dev-consumer

## Summary

Phase 83g closes the second consumer gap surfaced during the 83f
operator-validation work: the dev binary has no production consumer
for the Phase 28 MCP southbound driver. `cfg.Tools.MCPServers[]` is
declared in the config schema, exposed READ-ONLY through the Phase
73h Console `mcp.servers.*` Protocol methods, and validated at boot —
but nothing in `cmd/harbor/cmd_dev.go::bootDevStack` ever calls
`mcp.New` to spawn an MCP server subprocess, connect, discover tools,
or register them into the tool catalog. Configuring an `mcp_servers[]`
entry in `harbor.yaml` was silently ignored. 83g closes that: per
configured MCP server, the dev boot spawns the transport (stdio
subprocess or HTTP), opens the MCP session, discovers tools, registers
their descriptors into the catalog, and also registers the server with
the MCP Registry so the Console reflects the connection. Fail-loud at
boot when a configured server cannot connect or discover.

## RFC anchor

- RFC §6.4 — Tools subsystem (the southbound driver shape, tool
  catalog discipline, source-id prefix).
- RFC §6.16 — Agent Registry / discovery (the MCP Registry the
  Console reads from).

## Briefs informing this phase

- brief 03
- brief 14

## Brief findings incorporated

- brief 03 §4: the MCP southbound driver consumes operator config
  through `cfg.Tools.MCPServers[]`; the runtime is responsible for
  spawning + registering. 83g is the production consumer.
- brief 14 §1: the MCP client driver ships at Phase 28 but the
  southbound binary wiring is "deferred to a runtime-side phase."
  83g is that runtime-side phase for the dev binary; durable / multi-
  binary wiring lands later with the 85-band's distributed-bus work.

## Findings I'm departing from (if any)

None. 83g is a pure consumer phase against the already-shipped Phase
28 driver and the already-declared `cfg.Tools.MCPServers` schema.

## Goals

- The dev binary opens one `mcp.Provider` per `cfg.Tools.MCPServers[i]`
  at boot, connects, discovers tools, and registers each ToolDescriptor
  into the tool catalog.
- The same Provider is also registered with the MCP Registry so the
  Phase 73h Console MCP page reflects every connection (already
  exposed via the read-side Protocol methods).
- Boot fails loud (binary exits non-zero, error message naming the
  failing server) when a configured MCP server cannot connect or
  discover. No silent degradation; no quiet boot that hides a
  misconfigured / unreachable server.
- The driver Providers + Registry shut down cleanly on stack teardown
  (the existing `closeAll` chain) — no orphaned subprocesses.

## Non-goals

- No richer per-server options beyond the existing config schema
  (`OAuth`, `RetryPolicy`, etc.) — those land with the 85-band.
- No background reconnect / health-monitoring beyond what Phase 28
  already implements internally — operator gets the existing
  connect-once semantics.
- No A2A peer wiring — `cfg.Distributed.A2APeers` consumer is its
  own phase.

## Acceptance criteria

- [ ] `cmd/harbor/cmd_dev.go::bootDevStack` opens an `*mcp.Provider`
      per `cfg.Tools.MCPServers[i]` via `mcp.New(...)`, calls
      `Connect(ctx)`, calls `Discover(ctx)`, and registers each
      returned `ToolDescriptor` on the tool catalog.
- [ ] The same Providers are registered with an `mcp.Registry`
      built at boot. Mounting the Registry on the Protocol mux via
      `mcp.NewRegistryAccessor` + `protocol.NewMCPSurface` requires a
      single `*auth.Provider` accessor (the OAuth side); plumbing
      that is its own follow-up (the dev binary's OAuth providers
      land as a slice through `applyToolCatalogWiring`). 83g
      constructs the Registry and registers Providers into it so the
      Console MCP-page mount lands as a small follow-up; for V1.1
      operator value the catalog wiring (Providers reaching the tool
      catalog so the planner can call MCP tools) is what unblocks the
      chat path. Documented under risks/open-questions.
- [ ] Boot fails loud (`return nil, fmt.Errorf("mcp[%s]: %w", ...)`)
      when a configured MCP server's `Connect` or `Discover` fails.
- [ ] All Providers and the Registry land in the `closers` chain so
      stack teardown drains every subprocess.
- [ ] A new `cmd/harbor-mcptest-stdio/` binary implements a minimal
      MCP stdio server (one tool — `echo`) so the integration test
      can exercise the full config → subprocess → discover → register
      path without a third-party dependency.
- [ ] A new `test/integration/phase83g_mcp_dev_consumer_test.go`
      boots the dev stack with one configured MCP server pointing at
      the built test binary, asserts the `echo` tool is registered
      on the catalog, AND asserts the MCP Registry exposes the
      server. A separate sub-test asserts the fail-loud path with an
      unreachable command (`/nonexistent`).
- [ ] `scripts/smoke/phase-83g.sh` asserts the config schema is wired,
      the dev binary references `mcp.New` / `mcp.NewRegistry`, and
      `examples/harbor.yaml` documents an `mcp_servers[]` entry.
- [ ] The `harbortest/devstack` mirror lands the same wiring per
      D-094 source-of-truth.

## Files added or changed

- `cmd/harbor/cmd_dev.go` — open MCP Providers + Registry, register
  descriptors into the catalog, thread into the closer chain.
- `cmd/harbor-mcptest-stdio/main.go` — minimal stdio MCP server for
  the integration test.
- `cmd/harbor-mcptest-stdio/README.md` — explains the binary's role
  (test fixture only; never shipped in releases).
- `harbortest/devstack/devstack.go` — D-094 mirror of the
  cmd_dev.go MCP wiring.
- `examples/harbor.yaml` — document one stdio + one HTTP `mcp_servers[]`
  example (presently the block is `mcp_servers: []`).
- `test/integration/phase83g_mcp_dev_consumer_test.go` — the binding
  integration test.
- `scripts/smoke/phase-83g.sh` — static-only assertions.
- `docs/plans/README.md` — Phase 83g row + flip Status to Shipped.
- `docs/decisions.md` — D-150 (the consumer-side shape: where, when,
  fail-loud).

## Public API surface

None new at the Go-package boundary. 83g consumes existing
`internal/tools/drivers/mcp` and existing `cfg.Tools.MCPServers`
config. The Console-side methods already shipped in Phase 73h.

## Test plan

- **Unit:** N/A — 83g is a wiring phase against already-tested
  surfaces (Phase 28 unit tests cover the Provider; Phase 73h tests
  cover the Console-side surface).
- **Integration:** `test/integration/phase83g_mcp_dev_consumer_test.go`
  — boots the dev stack with one stdio MCP server backed by the
  built `harbor-mcptest-stdio` binary. Asserts the test tool reaches
  the catalog AND the Registry. A second sub-test asserts the fail-
  loud branch (unreachable command path).
- **Failure-mode:** the fail-loud sub-test above is the §17.3
  failure-mode coverage.
- **Concurrency / leak:** the existing per-Provider concurrent-reuse
  test in `internal/tools/drivers/mcp/` covers the Provider's D-025
  contract. 83g adds no new mutable artifact.
- **Operator validation (post-merge, manual):** resume the v1.1
  validation by booting `harbor dev` against the
  `~/harbor-validation/media-helper-agent` config (with its
  `mcp_servers[]` pointing at `uvx mcp-youtube`). The discovered
  `youtube_*` tools must appear in the catalog; the planner must be
  able to call them through a real Console message.

## Smoke script additions

`scripts/smoke/phase-83g.sh` (static-only) asserts:

- `cmd/harbor/cmd_dev.go` references `mcp.New(` (the production
  consumer wiring lands).
- `cmd/harbor/cmd_dev.go` references `mcp.NewRegistry(` (the
  Console-facing Registry is constructed).
- `cmd/harbor-mcptest-stdio/main.go` exists (the integration test's
  stdio server binary).
- `examples/harbor.yaml` carries a real `mcp_servers[]` example
  (no longer the empty list).

## Coverage target

- `cmd/harbor`: 80% (the boot path's MCP branch is exercised by the
  integration test).

## Dependencies

- Phase 28 (`internal/tools/drivers/mcp` — Provider + Registry).
- Phase 73h (the Console MCP page's Protocol surface — already wired
  to read from a `mcp.Registry`).
- Phase 26 (the tool catalog the Providers register into).

## Risks / open questions

- **Fail-loud on connect/discover may surprise operators with flaky
  MCP servers.** A future knob (per-server `optional: true` or a
  `--skip-mcp-on-error` CLI flag) could allow graceful degradation,
  but V1.1 ships strict fail-loud — matches the §13 / §5 posture and
  the 83f convention. Documented in D-150.
- **`uvx`-backed stdio servers fetch packages on first run.** A
  cold-cache first boot may stall longer than the default Connect
  timeout. Documented in `examples/harbor.yaml`.
- **No retry / reconnect loop at the dev binary level.** A Provider
  whose session drops mid-run surfaces as tool-call failures the
  planner sees. Phase 28's internal `KeepAlive` ping is the only
  liveness signal. Re-Connect on drop is a follow-up.

## Glossary additions

None.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Cross-session isolation test — N/A; MCP Providers are bound
      at boot under the dev-token identity, not per-session
- [ ] N/A — 83g adds no new reusable artifact (the Provider is
      already covered by Phase 28's D-025 tests)
- [ ] Integration test exists per CLAUDE.md §17 —
      `test/integration/phase83g_mcp_dev_consumer_test.go`
- [ ] No new vocabulary
- [ ] If a brief finding was departed from: justified above +
      decisions.md entry filed
