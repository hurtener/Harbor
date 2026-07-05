# Phase 156 — `agent_config.remove_mcp_connection` + detach-on-reconcile

## Summary

D-240 decision 5 deliberately deferred connection removal ("pausing is the revoke path"), with the recorded revisit condition: "revisited if a removal need emerges that pause cannot serve." That need has emerged — a coordinator control plane's delete flow must actually REMOVE a runtime-added MCP connection (deregister its tools, stop re-attaching it), which pause cannot express (a paused connection re-appears on resume and its descriptor persists forever). This phase ships the `agent_config.remove_mcp_connection` verb (a revision whose connections section drops the named descriptor, carrying every sibling section forward per Phase 152's completeness guard) and the detach leg of run-start reconciliation: a connected-but-no-longer-declared server is deregistered from the catalog + MCP registry at the next-turn projection boundary — which also closes D-240's deferred rollback gap (rolling back past an add now detaches through the same reconcile path).

## RFC anchor

- RFC §6.16
- RFC §6.4
- RFC §3.3

## Briefs informing this phase

- brief 09
- brief 14

## Brief findings incorporated

- brief 09 §5: connection lifecycle transitions ride the config-revision surface so every add/remove is diffable, auditable, and rollback-able — removal must be a revision, never an imperative side-channel teardown.
- brief 14 §4: MCP transport teardown is graceful and never yanks a transport mid-call — the next-turn projection boundary (not the verb handler) is where detach executes, matching the warm-transport model.

## Findings I'm departing from (if any)

- None. (D-240 decision 5's deferral is superseded through its own recorded revisit clause — a removal need pause cannot serve — recorded as D-287; this is the sanctioned path, not silent drift.)

## Goals

- `agent_config.remove_mcp_connection` removes a named connection descriptor as a new revision: fail-loud if the name is unknown; the SAME revision prunes the removed server's tool-exposure residue (its paused/disabled/loading entries) so no stale keys linger; all sibling sections (incl. `Hooks`) carried forward.
- Run-start reconciliation gains the detach leg: declared-set vs attached-set diff detaches undeclared servers — deregistering their tools from the planner catalog view and the MCP registry, and tearing the transport down gracefully at a run-start reconcile (never in the middle of the run that triggered it). **Honest in-flight semantics (amended by the PR-464 adversarial review):** EXPOSURE correctness is next-turn and independent of teardown (a removed server never appears in any catalog view projected after the removal revision — D-025/D-234); TEARDOWN is process-global (the catalog + registry are shared across sessions), so a different session's in-flight run whose next step calls the detached server fails LOUDLY — a typed catalog not-found at dispatch or a closed-transport error — never a hang, a panic, or a silent success (test-pinned). A refcount/drain protocol was considered and rejected (D-287 call 2 as amended).
- Rollback to a pre-add revision detaches through the identical reconcile path (one mechanism, no parallel implementation — §13).
- A canonical `mcp.connection.removed` event emits per removal (SafePayload), alongside the existing `agent.config.revised`.

## Non-goals

- No removal of CONFIG-declared (yaml) servers via the verb — those are boot config, not revisioned state; the verb rejects a name that resolves to a boot-declared server with a distinct loud error (the operator edits yaml + restarts for those).
- No agent-bound sealed-token deletion on remove: the token store entry persists so a re-add reuses the completed consent (re-add is a first-class flow, see the reconciliation phase); real credential revocation is provider-side. Documented in godoc + the skill; a `revoke` surface is a named follow-up if the need emerges.
- No draining protocol, no refcounting: teardown fires at a run-start reconcile, never in the middle of the run that triggered it, and a cross-session in-flight caller of a detached server fails loudly (see Goals — honest in-flight semantics). Exposure stays next-turn per the warm-transport model.

## Acceptance criteria

- [x] The verb: request/response wire types, method name in `methods.go`, service handler with identity/lock/validation symmetrical to `add_mcp_connection`; unknown name → loud typed error (`ErrConnectionNotFound` → 404), no revision, no event; boot-declared name → distinct loud typed error (`ErrBootDeclaredConnection` → 400).
- [x] The removing revision drops the descriptor, prunes the server's tool-exposure residue, and carries all sibling sections forward — the Phase 152 rebuild-completeness guard extended to cover this setter (one table row) proves it mechanically.
- [x] Reconcile detach leg: after remove (or rollback past an add), the next run's projected catalog contains none of the server's tools, the MCP registry no longer lists it, and the transport is closed (goroutine baseline asserted); a concurrent-run test proves race-freedom (`TestE2E_AgentConfig_RemoveConnection_*`). NOTE: 92o's run-start reconcile never shipped (parked band) — this phase BUILDS the reconcile mechanism, detach-only (§4.3 deviation, D-287 as-built note).
- [x] Remove wins over pause: a removed server's exposure residue is pruned in the same revision, so no `paused_servers` entry survives to re-expose it (`TestRemoveMCPConnection_DropsDescriptor_PrunesResidue_CarriesSiblings`).
- [~] Re-add after remove works end-to-end (`TestE2E_..._DetachOnReconcile` re-add leg); token-reuse-without-second-consent is structurally guaranteed (D-287 call 3 — the verb never touches the token store) but the full OAuth-consent fixture flow is deferred with the parked 92m/n band (§4.3).
- [x] `mcp.connection.removed` canonical event registered + emitted; D-209 docs regen (`make protocol-docs-gen`); full D-223 lockstep for the new verb + types (`make protocol-ts-gen` + `-check`). `docs/skills/` carries no `agent_config.*` connection-verb surface (§18 exempt).
- [x] Cross-agent + cross-tenant isolation: the verb + reconcile are identity-scoped by the caller triple (unit + E2E). `agent_id` is registration metadata, never an isolation filter.
- [x] `scripts/smoke/phase-156.sh` OK ≥ 2, FAIL = 0; prior smokes (esp. 92-band + 152) pass.

## Files added or changed

- `internal/runtime/agentcfg/protocol/removeconnection.go` (new) + `methods.go` + wire types.
- `internal/agentcfg/` — connections-section diff arm (`removed` names) if not already expressible.
- `internal/runtime/agentcfg/projection/` — the reconcile detach leg.
- Catalog/MCP-registry deregistration seams under `internal/tools/` as required.
- `web/console/src/lib/protocol/*` + manifest — D-223; `docs/site/protocol/*` — D-209.
- `internal/runtime/agentcfg/protocol/rebuild_completeness_test.go` — extended.
- `scripts/smoke/phase-156.sh`.

## Public API surface

- The `agent_config.remove_mcp_connection` Protocol method (request: agent id + connection name; response: revision view). Everything else stays internal.

## Test plan

- **Unit:** validation legs (unknown name, boot-declared name, empty name), residue pruning (incl. the sibling-prefix regression: removing `git` with `git_hub` still declared must not prune `git_hub_clone` — a policy downgrade otherwise), diff arm, event emission.
- **Integration:** real MCP stdio fixture (`cmd/harbor-mcptest-stdio`, §17.8): add → tools visible → remove → next-turn catalog excludes + registry empty + transport closed; rollback-past-add leg through the same assertions; re-add-reuses-token leg; failure mode = remove of unknown name; in-flight loud-failure shape (a run mid-flight whose server is removed + reconciled from another session sees its next call fail with a typed error — never hang/panic/silent success).
- **Conformance:** N/A — no new driver seam.
- **Concurrency / leak:** N≥10 concurrent reconciles during a remove — converged final state, next runs excluded, `-race` clean, goroutine baseline after transport teardown (the leak test IS the teardown proof).

## Smoke script additions

- live-server: `agent_config.add_mcp_connection` (fixture) → `remove_mcp_connection` → `get` asserts descriptor gone + exposure residue pruned; remove of unknown name asserts the typed error shape. skip_if_404 throughout.

## Coverage target

- `internal/runtime/agentcfg/protocol`: 85%; `internal/runtime/agentcfg/projection`: 80%

## Dependencies

- 152 (hooks carry-forward + the completeness guard this setter joins), 92m (add-connection), 92n (resume-completes-attach), 92o (run-start reconciliation — the mechanism gaining the detach leg), 118 (D-223 gate).

## Risks / open questions

- Transport teardown ordering vs. a run that starts DURING the teardown — reconcile holds NO cross-run lock (the per-`(tenant, agentID)` write lock serialises the VERB's read-modify-write only, not run-starts); safety comes from the atomic active-revision read, the idempotent detacher (an already-gone source is a no-op), and each primitive (catalog deregister, registry deregister, transport close) being internally synchronised, so N concurrent reconciles converge. The concurrency tests target exactly this window. Two accepted residual windows until the attach leg lands (code-commented + D-287 as-built note): a reconcile racing a concurrent re-add can detach the freshly re-added server (heals at the next add/restart), and `AttachedSources()` is process-global (the multi-agent attach leg must scope it per agent).
- Whether the MCP registry deregistration needs a Console-visible transitional state ("removing") — V1 answer: no; next-turn semantics make removal atomic from the observer's view. Revisit only with operator evidence.

## Glossary additions

- Detach-on-reconcile (added to `docs/glossary.md` in this PR).

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes
- [x] Concurrent-reuse: remove-under-load stress N≥150 mixed runs/edits under `-race` (`TestReconcileConnections_RemoveUnderLoad_Stress`); N=128 concurrent reconciles + N=16 concurrent E2E reconciles.
- [x] Integration test wires real drivers + a real stdio MCP subprocess end-to-end, asserts identity propagation, covers ≥1 failure mode (unknown / boot-declared name), runs under `-race`
- [x] If new vocabulary: glossary "Detach-on-reconcile" present
- [x] Findings departed from: the parked 92o attach leg — justified in D-287's as-built note + §4.3
