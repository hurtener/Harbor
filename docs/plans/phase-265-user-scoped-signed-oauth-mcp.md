# Phase 265 — User-scoped signed OAuth MCP capability lifecycle

## Summary

Add a generic user-tier sibling to the existing signed OAuth MCP capability
registration and removal lifecycle. The sibling keeps the authority envelope
and connection descriptor closed, derives all identity scope from the verified
bearer, and stores the user's desired pair in `ConfigScopeUser`.

## RFC anchor

- RFC §5.5
- RFC §6.4
- RFC §6.11
- RFC §6.16

## Decision

- D-448 — user-scoped signed OAuth MCP capability lifecycle.

## Dependencies

- D-397, D-398, D-401, and D-407 signed OAuth MCP lifecycle, provider, and
  reach contracts.
- Existing closed authority-envelope and connection-descriptor wire types.
- Existing MCP attach/discover/provider and agent-config revision seams.

## Goals

- Expose user-tier register/remove Protocol methods with generated Go and
  TypeScript surfaces.
- Require a verified identity, `agent_config:user`, and signed reach to the
  target agent before user lifecycle work begins.
- Derive tenant, user, and session from the verified bearer and bind the
  operation, desired pair, physical owner, and OAuth provider to that scope.
- Keep `ConfigScopeAgent` as the operator base and persist user desired pairs
  only in `ConfigScopeUser`.
- Preserve user-owned signed pairs across generic user configuration writes.
- Project only the acting user's desired user-owned MCP pairs and tool
  narrowing, while retaining the operator/boot ceiling.
- Prove same-agent two-user registration and removal isolation, including
  colliding signed authority JTIs.
- Reuse the process-global MCP registry with a server-derived owner-scoped
  physical source key for user-owned attachments; logical descriptor names
  remain the desired-state and policy names, while operator/boot names stay
  unchanged.
- Ensure private OAuth initialize and discovery use the acting verified
  identity on every connection path without widening shared token destinations.

## Non-goals

- Changing the closed authority envelope or connection descriptor.
- Adding a user-selected provider, endpoint, token sink, tenant, session, or
  authorization field to the wire contract.
- Changing operator/boot bare-name behavior or allowing a client-selected
  physical source, provider, endpoint, or token destination.
- Supporting a non-OAuth user attachment in this contract. The user sibling is
  deliberately OAuth-only; a missing/empty authority envelope is rejected by
  the existing signed lifecycle rather than falling back to an unsigned or
  generic user attach path.
- Making user-owned MCP registrations visible to operator/agent scope as
  desired state, or widening the operator base through a user write.
- Adding a new OAuth provider store, credential vault, token exchange, or
  background worker.

## Contract

`agent_config.user.register_oauth_mcp_capability` and
`agent_config.user.remove_oauth_mcp_capability` are additive user-tier
methods. Their request bodies use the same closed descriptor and signed
authority envelope as the operator lifecycle. The service authenticates the
verified identity and reach before it resolves or mutates a revision.

The user revision key is `(tenant, user, agent)` and the pair itself retains
the issuing `(tenant, user, session, agent)` binding. Replay identity extends
the existing operation tuple with verified user and session only for the user
tier; operator replay behavior remains unchanged. Removal requires the exact
expected revision/content hash, signed pair identity, and physical owner.

The effective projection starts from the existing operator/boot ceiling and
then admits only the acting user's desired pair names. A physical source owner
with a different user is excluded from that acting-user view. Boot-declared
and operator-owned entries retain their existing shared visibility and
authorization gates.

The MCP registry remains process-global, but user-owned attachments are stored
under a deterministic server-derived physical source key computed from the
logical descriptor name and the verified owner. The logical name is retained
separately for desired-state matching and user tool-exposure policy; it is not
used as a shared physical key. Operator/boot names remain unchanged. User-owned
attachments and private providers carry an exact owner tag. Exact owner and
descriptor comparisons guard detach, teardown, replacement, reattach, and
backoff. The physical mapping is server-only and never selects a downstream
URL, provider, bearer, or token sink. Pair-owned providers use
`OwnOAuthProvider`; the existing driver resolves the bearer from the acting
context before initialize and discovery.

## Acceptance criteria

- [x] User register/remove methods are present in canonical methods, stream
      routes, Go wire types, TypeScript client/types, and generated references.
- [x] User lifecycle rejects missing/unverified identity, missing
      `agent_config:user`, and missing signed reach.
- [x] Desired user state is written/read only under `ConfigScopeUser`; the
      operator scope remains unchanged by a user registration.
- [x] Two users sharing one tenant and agent have independent desired pairs,
      physical owners, replay slots, and removals.
- [x] Effective user projection admits the acting user's source and excludes
      a foreign user source while retaining the operator/boot ceiling.
- [x] Generic user configuration writes carry the signed pair forward.
- [x] Physical MCP owner and exact teardown paths include the user identity;
      the same logical descriptor can map to distinct physical sources without
      a client-selected destination.
- [x] OAuth provider ownership remains private to the pair and does not widen
      the shared provider set or token destination.
- [x] Focused source tests, generator lockstep checks, static smoke, and the
      focused race gate pass locally.

## Files added or changed

- Signed OAuth user service, scope authorization, replay identity, and
  reconciliation in `internal/runtime/agentcfg/protocol/`.
- User-aware physical MCP owner and source projection seams in
  `internal/tools/auth/`, `internal/tools/drivers/mcp/`, and
  `internal/runtime/serve/`.
- Canonical Protocol methods/types, stream routes, TypeScript client/types,
  and generated manifest/reference files.
- Two-user lifecycle and effective projection tests.
- D-448, this phase plan, and `scripts/smoke/phase-265.sh`.

## Test plan

- `go test ./internal/runtime/agentcfg/protocol -run 'TestUserSignedOAuthMCPCapability'`.
- `go test ./internal/runtime/agentcfg/projection -run 'TestActivePlannerCatalogView_UserMCP'`.
- Focused `go test` for serve, MCP driver, stream, methods, and single-source
  packages.
- `go test -race` over the complete focused lifecycle/projection/serve/MCP
  set.
- Protocol TypeScript, reference, and committed-file lockstep checks.
- Static phase smoke assertions and shell syntax validation.

## Smoke script additions

`scripts/smoke/phase-265.sh` asserts the user methods/types/routes, verified
authorization, `ConfigScopeUser`, full physical owner plumbing, user-only
projection, private OAuth provider ownership, the two-user tests, D-448, and
the generated contract surfaces. It is static-only and does not claim a live
runtime or hosted CI result.

## Coverage target

Every new authorization, scope, owner, projection, route, and isolation branch
has a focused test. No unrelated package coverage claim is made.

## Risks and deliberate boundaries

- The registry remains process-global, but user-owned logical names are mapped
  to deterministic owner-derived physical source keys. Physical ids stay in
  the runtime registry/catalog only; desired pairs and exposure policy remain
  logical. A collision with an already-occupied derived key still fails loud.
- A pair is user desired state but remains bound to the session that issued
  its signed authority. A later session for the same user cannot remove it
  with a mismatched physical owner or signed binding.
- This phase intentionally supports only the signed OAuth descriptor/envelope
  contract. Non-OAuth user attachment needs a separate closed descriptor and
  signer-authorized method; no empty-envelope or browser-selected fallback is
  permitted.
- Existing operator maintenance preserves its compatibility fence and skips
  user operation receipts; user reconciliation is separately keyed by user
  and session.

## Public surface

- `AgentConfigUserRegisterOAuthMCPCapabilityRequest` / `Response`.
- `AgentConfigUserRemoveOAuthMCPCapabilityRequest` / `Response`.
- `agent_config.user.register_oauth_mcp_capability`.
- `agent_config.user.remove_oauth_mcp_capability`.
- `ProtocolClient.userRegisterOAuthMCPCapability` and
  `ProtocolClient.userRemoveOAuthMCPCapability`.

Protocol version remains `0.1.0`.

## Evidence status

Local focused tests, generated-source checks, the focused race gate, and the
static smoke pass at the implementation head. No hosted CI, release tag,
deployment, or downstream acceptance is claimed.
