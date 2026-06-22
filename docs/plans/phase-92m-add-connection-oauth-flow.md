# Phase 92m — add_mcp_connection OAuth config + InitiateFlow parking

## Summary

Wires `agent_config.add_mcp_connection` into Harbor's existing agent-bound tool-side OAuth primitive. The request gains an OPTIONAL `OAuth` block; on a typed `ErrAuthRequired` from the attach, the service registers the server's `OAuthConfig` (the runtime config seam) and drives `InitiateFlow` on the unified pause/resume Coordinator — REPLACING the dead-end `parkForAuth` with a real, correlated OAuth-flow pause. The response carries the `authorize_url` + `pause_token` so an operator can complete consent out-of-band. See the wave decomposition `docs/plans/wave-mcp-oauth-decomposition.md` (§2, §3 "92m") for the end-state this phase delivers.

## RFC anchor

- RFC §6.4 — Tool catalog and transports (the runtime MCP OAuth surface this drives).
- RFC §6.16 — Agent Registry (the agent-bound binding for a runtime-added connection).
- RFC §3.3 — the unified pause/resume primitive (the parking substrate the OAuth flow uses).

## Briefs informing this phase

- brief 09
- brief 14

## Brief findings incorporated

- **brief 09 (MCP OAuth from bifrost):** an agent-shared MCP server authenticates with an agent-bound token keyed by the registration `agent_id` — never a user-bound credential. This phase routes the add-connection OAuth need through `BindingScope == ScopeAgent`, so a token authorized once is reused across the agent's sessions; the binding scope is agent by construction (no per-request scope choice).
- **brief 09:** the OAuth need must park on the ONE pause/resume path and resume from the real callback, not a bespoke wait. This phase replaces `parkForAuth` (a bare park nobody can complete) with `InitiateFlow`, whose pause is correlated to `CompleteFlow` via the flow `state`.
- **brief 14 (MCP client/host compliance):** operator-supplied OAuth configuration (client_id / scopes / endpoints / redirect_uri) is the explicit path; spec discovery is the fallback. This phase ships the operator-supplied `OAuth` block; the 401 → metadata-discovery path is a later phase, so most block fields are validated-if-present, optional otherwise.

## Findings I'm departing from (if any)

None.

## Goals

- `AgentConfigAddMCPConnectionRequest` gains an OPTIONAL `OAuth` block — the operator-supplied OAuth descriptor (client_id, scopes, server_url OR authorize+token URLs, redirect_uri). Binding scope is agent by construction; the operator never chooses it.
- On a typed `ErrAuthRequired` from the attach, the service `RegisterConfig`s the server's `OAuthConfig` (the runtime config-registration seam) and calls `InitiateFlow` — the pause is the OAuth-flow pause correlated to `CompleteFlow`, not a bare park.
- The response returns the `authorize_url` + the `pause_token` an operator drives to complete consent.
- Secrets (`client_secret`, any header) flow to the provider/transport ONLY — never persisted in the recorded revision, the diff, or any emitted event (CLAUDE.md §7). The non-secret descriptor invariant is unchanged.
- Admin-scoped: a `ScopeAgent` `InitiateFlow` requires the control scope (the provider enforces the admin-scope error); the gate is fail-closed.

## Non-goals

- The resume bridge that re-drives the attach to `online` (a separate phase — the consumer of this phase's pause).
- Run-start connection reconciliation (a separate phase).
- Spec-faithful 401 → metadata discovery (a separate phase; here the OAuth block is operator-supplied).
- The Console advisory + the wave-end live E2E (a separate phase).
- stdio OAuth — stdio is not an OAuth transport; the OAuth block is the http path only.

## Acceptance criteria

- [ ] The OPTIONAL `OAuth` block is validated when present (binding scope agent; client_id / scopes; server_url OR authorize+token URLs; redirect_uri); an incoherent block fails loud with the invalid-connection error.
- [ ] On a typed `ErrAuthRequired`, the service registers the server's `OAuthConfig` via the runtime config seam and drives `InitiateFlow` (NOT `parkForAuth`); the pause is correlated to the OAuth flow.
- [ ] The `auth_required` response carries the `authorize_url` AND the `pause_token`.
- [ ] Secret hygiene preserved: a test asserts NO secret (`client_secret`, header value) appears in the recorded revision, the diff, or any emitted event.
- [ ] The new wire fields are documented in the example config AND exercised by `scripts/smoke/phase-92m.sh`.
- [ ] Identity-mandatory (the verified triple); admin-scoped (authority from the verified ctx, never the body); the `ScopeAgent` `InitiateFlow` admin-scope gate is fail-closed.

## Files added or changed

- `internal/protocol/types/agentconfig.go` — add the optional `OAuth` block to `AgentConfigAddMCPConnectionRequest` + the `authorize_url` field on the response.
- `internal/protocol/{methods/methods.go,singlesource/singlesource.go}` + the generator typeindex files — register the reshaped wire types (TS manifest + typed client + generated docs regenerated).
- `internal/runtime/agentcfg/protocol/addconnection.go` — replace `parkForAuth` with the `RegisterConfig` + `InitiateFlow` drive; project the OAuth block onto the provider config; carry the authorize URL onto the response.
- `internal/runtime/agentcfg/protocol/` — the OAuth-block validation + the provider-config projection helpers.
- `cmd/harbor/cmd_dev.go` + `harbortest/devstack/devstack.go` — inject the OAuth provider (the config seam + `InitiateFlow`) into the add-connection service (the devstack twin, so the two cannot drift).
- `web/console/src/lib/protocol/agentconfig.ts`; `docs/site/protocol/*`; `examples/*.yaml` (the OAuth-block example); `scripts/smoke/phase-92m.sh`.

## Public API surface

```go
// Protocol: agent_config.add_mcp_connection now accepts an optional OAuth
// block (operator-supplied: client_id / scopes / server_url or
// authorize+token URLs / redirect_uri). On an auth-required attach the
// service registers the server's OAuth config and initiates the flow; the
// response carries authorize_url + pause_token. Secrets reach the provider
// and transport only — never the recorded revision, the diff, or an event.
```

## Test plan

- **Unit:** the OAuth block validates (coherent block accepted; missing-endpoints block rejected; secret fields accepted but never echoed); a typed auth-required attach drives `RegisterConfig` + `InitiateFlow` (NOT `parkForAuth`); the response carries `authorize_url` + `pause_token`; a non-admin / non-control-scope `ScopeAgent` flow is rejected fail-closed; an add with no OAuth block and no auth requirement still attaches online.
- **Integration:** `test/integration/agentcfg_add_connection_oauth_test.go` — the add-connection service + the real OAuth provider (the config seam + the unified Coordinator) + a real bus + registry: an attach that surfaces the typed auth-required error registers the config, initiates the flow, parks on the Coordinator, and returns the authorize URL; assert NO secret reaches the revision / diff / events; identity propagation across the seam; ≥1 failure mode (missing Coordinator → loud error); under `-race`.
- **Conformance:** reuses the agentcfg driver conformance (revision recording).
- **Concurrency / leak:** concurrent adds against one shared service + provider under `-race`; no flow-record leak; baseline goroutines restored.

## Smoke script additions

- `scripts/smoke/phase-92m.sh` (live-server): exercises the `agent_config.add_mcp_connection` verb with an OAuth block — asserts the `auth_required` response shape carries `authorize_url` + `pause_token`, the non-admin path is rejected, and (static greps) the OAuth-block wire fields + the example-config entry + the typed-client + generated-docs rows are present. The script stays `skip` until the phase ships; the add verb returns 404/405/501 → SKIP on a pre-phase build (the 404/405/501 → SKIP convention), flipping to OK ≥ the auth-required assertions once the surface lands.

## Coverage target

- `internal/runtime/agentcfg/protocol`: 85%

## Dependencies

- 92f (the add-connection lifecycle + the non-secret descriptor invariant), 92k (the runtime OAuth config-registration seam), 92l (the MCP transport agent-bound OAuth + the typed auth-required error this branches on).

## Risks / open questions

- **Secret leakage surface.** The OAuth block carries `client_secret` and the request carries operator headers — both secrets. The risk is a secret reaching a revision / diff / event / log. Mitigated by: the existing non-secret-descriptor projection (secrets never enter the recorded payload), a dedicated secret-hygiene test asserting absence across all three sinks, and the existing reason-scrubbing pass.
- **Binding-scope ambiguity.** The block must be agent-bound by construction; a request that tries to widen the scope is a design smell. Pinned by the wire shape (no scope field on the block) + a validation test.
- **Pause without its completer.** This phase parks; the resume bridge that re-drives the attach is the next phase. Until it lands the pause releases without completing the attach (the prior as-built behavior). Documented in non-goals + the wave doc; not a silent degradation (the response is explicit about the `auth_required` state).

## Glossary additions

- None (reuses runtime MCP attach, agent-bound token, unified pause/resume — all already in the glossary).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **If this phase builds a reusable artifact: concurrent-reuse test passes (N≥100 against one shared instance under `-race`).** The add-connection service + provider seam are exercised by the concurrency test above.
- [ ] **Integration test exists, real OAuth provider + Coordinator + bus + registry on the seam, identity propagation, ≥1 failure mode (missing Coordinator / non-admin), `-race`.**
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
