# Phase 92n — Resume-completes-attach bridge (closes #375 gap 1)

> Part of the parked **Runtime MCP tool-side OAuth** wave — see
> `docs/plans/wave-mcp-oauth-decomposition.md` (§2 end-state, §3 "92n", §4 staging,
> §7 risks) and the umbrella decision **D-240**. This phase is the gap-1 mechanism:
> the resume that finally re-drives the parked attach to `online`.

## Summary

Closes #375 gap 1: the `auth_required` resume is a dead-end today (resume only
releases the pause; the MCP server never comes online). This phase wires a
long-lived agent-config component that subscribes to the bus for `pause.resumed`
(`EventTypePauseResumed`); when a resumed token corresponds to an `mcp_oauth`
add-flow pause it re-drives the attach via 92l's token-aware path under the
per-`(tenant, agentID)` write lock, taking the server `online` (terminal
`mcp.connection.added`) or failing loud (`mcp.connection.failed`, server stays
offline — never a silent re-park or drop). It re-instates the "a resume completes
the attach" claim the checkpoint PR downgraded.

## RFC anchor

- RFC §3.3 — the unified pause/resume primitive (the parking + resume substrate
  the bridge reacts to; the resume is the continuation that completes the attach).
- RFC §6.16 — Agent Registry (the agent-bound binding for a runtime-added
  connection; the re-driven attach reads the agent-bound token keyed by the
  registration `agent_id`).

## Briefs informing this phase

- brief 09
- brief 14

## Brief findings incorporated

- **brief 09 (MCP OAuth from bifrost):** an agent-bound token completes the
  authorization need; the resume that lands the token is what makes the connection
  usable. This phase re-drives the attach on `pause.resumed` so the now-present
  agent-bound token (persisted by `CompleteFlow`) is read by 92l's transport path
  and the server comes online — the resume is no longer a dead-end.
- **brief 14 (MCP client/host compliance):** a connection that cannot complete its
  auth handshake must not be presented as live. The bridge fails loud
  (`mcp.connection.failed` + logged error, server stays offline) on a re-drive
  failure rather than reporting a half-authorized server — a half-attached server
  is never registered (CLAUDE.md §13).

## Findings I'm departing from (if any)

None.

## Goals

- A long-lived agent-config component subscribes to the bus for `pause.resumed`
  and recognises the `mcp_oauth` add-flow pauses it created (the token it minted on
  the park in 92m).
- On a matching resume, re-drive the attach via 92l's token-aware path under the
  per-`(tenant, agentID)` write lock (`s.lockAgent`) so a concurrent revise/add
  cannot race the resume's read-modify-write.
- Success → the server is `online` and a terminal `mcp.connection.added` lifecycle
  event fires (the descriptor revision was already recorded at park time in 92m).
- Re-drive failure → FAIL LOUD: a loud `mcp.connection.failed` event + a logged
  error; the server stays offline. NEVER a silent re-park, a silent drop, or a
  retry loop that hides the failure (CLAUDE.md §13).
- Re-instate the "a resume completes the attach" claim: revert the checkpoint
  downgrade in three places (D-237 §2, the `addconnection.go` + `methods.go`
  godoc, and the 92f plan) to the now-true wording.

## Non-goals

- Run-start connection reconciliation / the durable restart backstop (92o) — a
  pause whose process died is re-driven at the next run, not by this in-memory
  bridge.
- Spec-faithful MCP OAuth discovery (92p).
- Console advisory rendering / wave-end E2E (92q).
- Detach-on-rollback — deferred wave-wide (D-240 decision 5; pause/resume is the
  revoke path).

## Acceptance criteria

- [ ] **Headline gap-1 regression guard:** an `mcp_oauth` add-flow pause that is
  RESUMED drives the server to `online` (the bridge observes `pause.resumed`,
  re-drives the attach via 92l's token-aware path, and the server's tools register
  on the live catalog) — the exact regression #375 filed. Terminal
  `mcp.connection.added` fires.
- [ ] Re-drive failure fails loud: a `mcp.connection.failed` event + a logged
  error; the server stays offline; no silent re-park, no silent drop (CLAUDE.md
  §13).
- [ ] The re-drive runs under the per-`(tenant, agentID)` write lock: a concurrent
  revise (another admin write verb on the same agent) and the resume serialise —
  neither sees a torn read-modify-write (concurrent-revise-vs-resume test).
- [ ] Only the bridge's own `mcp_oauth` add-flow tokens are acted on: a
  `pause.resumed` for an unrelated reason (HITL approval, A2A) is ignored — no
  spurious attach.
- [ ] **Claim reinstatement #1:** D-237 §2's "As-built clarification" downgrade is
  reverted — "a resume completes the attach" is the true wording again.
- [ ] **Claim reinstatement #2:** the `addconnection.go` `ConnectionStateAuthRequired`
  godoc + the `methods.go` `MethodAgentConfigAddMCPConnection` godoc are reverted
  to "the resume re-drives the attach to online" (drop the "not yet implemented /
  issue #375" caveats).
- [ ] **Claim reinstatement #3:** the 92f plan
  (`docs/plans/phase-92f-agent-config-add-connection.md`) OAuth-path goal +
  acceptance criterion are reverted to the now-true wording (resume completes the
  attach), referencing 92n for the continuation.
- [ ] Restart caveat documented (not silently degraded): a pause that outlived its
  process is re-driven by 92o at the next run, NOT by the in-memory bridge.
- [ ] Identity scoped by the triple; the re-drive runs under the resumed pause's
  verified identity; the agent-bound token keys by the registration `agent_id`
  (NOT an isolation filter, §6).
- [ ] If the shared service path is touched: concurrent-reuse test passes (N≥100
  concurrent resumes against one shared bridge/service instance under `-race`).
- [ ] `scripts/smoke/phase-92n.sh` green.

## Files added or changed

- `internal/runtime/agentcfg/protocol/resumebridge.go` — the long-lived
  `pause.resumed` subscriber: an in-memory token → add-flow-context map (populated
  when `AddMCPConnection` parks in 92m), the bus subscription + dispatch loop, and
  the re-drive-under-write-lock + fail-loud terminal-event logic.
- `internal/runtime/agentcfg/protocol/addconnection.go` — re-instate the
  `ConnectionStateAuthRequired` godoc (drop the "#375 / not yet implemented"
  caveat); on park, register the token's add-flow context with the bridge.
- `internal/runtime/agentcfg/protocol/service.go` — hold the bridge (or its
  token-context registry) on the `Service`; reuse the existing `lockAgent`
  per-`(tenant, agentID)` write lock for the re-drive.
- `internal/protocol/methods/methods.go` — re-instate the
  `MethodAgentConfigAddMCPConnection` godoc (resume completes the attach).
- `cmd/harbor/cmd_dev.go` + `harbortest/devstack/devstack.go` — start the bridge
  subscription against the same bus + coordinator + attacher (D-094 twin, so the
  production run loop and the devstack twin cannot drift; the §17.6 one-binary-only
  failure mode is pre-empted).
- `docs/plans/phase-92f-agent-config-add-connection.md` — re-instate the
  resume-completes-attach wording (claim reinstatement #3).
- `docs/decisions.md` — D-237 §2 "As-built clarification" reverted (claim
  reinstatement #1); D-244 logged on ship (§17.7 step 3).
- `scripts/smoke/phase-92n.sh`.

## Public API surface

```go
// A long-lived bridge subscribes to pause.resumed; an mcp_oauth add-flow
// resume re-drives the attach under the per-(tenant, agentID) write lock.
//
//   type ResumeBridge struct{ /* token -> add-flow context, bus sub, attacher */ }
//   func (b *ResumeBridge) Start(ctx context.Context) error // subscribe + dispatch
//   func (b *ResumeBridge) register(token, addFlowContext)  // called on park
//
// The bridge is internal to the agentcfg protocol service; no new Protocol
// method or wire type lands (the resume travels the existing pause/resume
// surface). The reinstated claim is godoc + plan text, not a new API.
```

## Test plan

- **Unit:** an `mcp_oauth` `pause.resumed` re-drives the attach → `online` +
  terminal added event; a re-drive failure → loud failed event, server offline, no
  re-park; an unrelated `pause.resumed` (approve/reject/timeout, non-`mcp_oauth`
  reason) is ignored; the token-context map is populated on park and consulted on
  resume.
- **Integration:** `test/integration/agentcfg_resume_attach_test.go` — real bus
  (`events/drivers/inmem`), real pause/resume coordinator, real agentcfg registry,
  and a fixture `ConnectionAttacher` whose first call returns
  `ErrAuthRequired` and whose post-token call succeeds: park → resume the token →
  assert the server reaches `online` end-to-end and the descriptor revision is
  intact; assert identity propagation through every layer; ≥1 failure mode (the
  re-drive's second attach fails → `failed` recorded, offline); under `-race`.
- **Conformance:** reuses the 92a `agentcfg` driver conformance (no new driver).
- **Concurrency / leak:** the concurrent-revise-vs-resume race test (a resume and
  an admin revise on the same agent serialise on `lockAgent`); N≥100 concurrent
  `mcp_oauth` resumes against one shared bridge under `-race` (no data races, no
  context bleed, no cross-cancellation); the bridge's subscription goroutine is
  joined on shutdown — baseline `runtime.NumGoroutine` restored.

## Smoke script additions

- `scripts/smoke/phase-92n.sh`: live-server — drive an admin `add_mcp_connection`
  against a fixture server that parks `auth_required`, resume the returned pause
  token via the pause/resume surface, and assert the connection transitions to
  `online` (the gap-1 regression guard) with a terminal `mcp.connection.added`
  observed; a forced re-drive failure surfaces `mcp.connection.failed` (server
  stays offline). 404/405/501 → SKIP so the script coexists with pre-92n builds.

## Coverage target

- `internal/runtime/agentcfg/protocol`: 85%

## Dependencies

- 92m (the `InitiateFlow` parking + the token-context the bridge consumes), 50
  (the pause/resume coordinator + `EventTypePauseResumed`).

## Risks / open questions

- **Restart-survival of an in-flight add-flow pause.** The in-memory token-context
  map is lost when the process dies, so the bridge cannot re-drive a pause whose
  process exited. This is documented, NOT silently degraded: 92o's run-start
  reconcile is the durable backstop (the descriptor revision + the agent-bound
  token both survive a restart), and the gap is logged loud. See the wave doc §7
  risk 3.
- **Race between a concurrent revise and the resume.** Mitigated by re-driving
  under the same per-`(tenant, agentID)` `lockAgent` write lock every admin write
  verb already takes — pinned by the concurrent-revise-vs-resume test.
- **Acting on a foreign resume.** The bridge MUST act only on the `mcp_oauth`
  add-flow tokens it minted (matched via its own token-context map), never on every
  `pause.resumed` — an HITL/A2A resume must not trigger a spurious attach. Pinned
  by the "unrelated resume ignored" test.

## Glossary additions

- **resume-completes-attach bridge** — the long-lived agent-config component that
  subscribes to `pause.resumed` and, for an `mcp_oauth` add-flow pause, re-drives
  the runtime MCP attach to `online` (or fails loud) under the per-`(tenant,
  agentID)` write lock. The continuation that makes a parked OAuth add usable;
  distinct from the durable run-start reconcile (92o) that backstops a restart.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **If the shared service path is touched: concurrent-reuse test passes — N≥100
  concurrent resumes against one shared bridge/service instance under `-race`, no
  data races / context bleed / cross-cancellation / goroutine leaks.**
- [ ] **Integration test exists (`test/integration/agentcfg_resume_attach_test.go`):
  real bus + coordinator + registry + fixture attacher, identity propagation, ≥1
  failure mode (re-drive failure), `-race`.**
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
