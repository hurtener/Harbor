# Phase 265 — User-scoped signed OAuth MCP capability lifecycle

## Summary

Add a generic user-tier sibling to the existing signed OAuth MCP capability
registration and removal lifecycle. The sibling keeps the authority envelope
and connection descriptor closed, derives all identity scope from the verified
bearer, and stores the user's desired pair and tool choices in
`ConfigScopeUser`. It also exposes one recovery/read operation that invokes
the same user-tier signed-capability reconciler used at run start and returns
the fresh profile projection for an immediate retry.

## RFC anchor

- RFC §5.5
- RFC §6.4
- RFC §6.11
- RFC §6.16

## Briefs informing this phase

- brief 09 — signed MCP OAuth binding scopes and identity-mandatory
  authorization.
- brief 05 — the session identity triple and durable state boundaries.
- brief 06 — Protocol-client projections and server-enforced isolation
  filtering.

## Brief findings incorporated

- brief 09 §"What Harbor must add" items 2 and 4: the binding scope drives
  lookup and authorization behavior, and each flow must fail closed when its
  required identity components are missing. User registration and removal
  therefore derive scope from the verified bearer rather than accepting a
  caller-selected owner.
- brief 05 §1: session identity is the `(tenant_id, user_id, session_id)`
  triple. The user revision, physical owner, and projected catalog retain
  that boundary throughout lifecycle and read operations.
- brief 06 §"Isolation-triple filtering by default": filtering and
  cross-scope access are enforced at the runtime/Protocol boundary. The
  user projection consequently narrows the operator ceiling for the acting
  identity instead of creating a client-side or shared view.

## Findings I'm departing from (if any)

None.

## Decision

- D-448 — user-scoped signed OAuth MCP capability lifecycle.
- D-450 — user-tier live-profile reconciliation reuses the signed capability
  reconciler.

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
- Persist the bounded per-server and per-tool loading-mode choices
  (`always`/`deferred`) in the same user revision; compose them after the
  operator baseline without widening the operator capability ceiling.
- Project only the acting user's desired user-owned MCP pairs and tool
  narrowing/loading choices, while retaining the operator/boot ceiling and
  the session overlay's disable union.
- Prove same-agent two-user registration and removal isolation, including
  colliding signed authority JTIs.
- Reuse the process-global MCP registry with a server-derived owner-scoped
  physical source key for user-owned attachments; logical descriptor names
  remain the desired-state and policy names, while operator/boot names stay
  unchanged.
- Ensure private OAuth initialize and discovery use the acting verified
  identity on every connection path without widening shared token destinations.
- Provide `agent_config.user.reconcile_live_profile` as the canonical
  immediate retry operation. It must reuse the existing
  `SignedOAuthMCPReconciler` `ConfigScopeUser` path and return a fresh active
  revision without introducing state, idempotency, or a second lifecycle.

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
tier; operator replay behavior remains unchanged. The issuing session remains
audit/replay metadata and is not required for a later session to use,
reconcile, or remove the durable pair. Removal requires the exact expected
revision/content hash, signed pair identity, and physical owner.

The effective projection starts from the existing operator/boot ceiling and
then applies the acting user's durable loading-mode choices and desired pair
names. A physical source owner with a different user is excluded from that
acting-user view. Loading choices are translated from logical connection/tool
names to owner-derived physical source names before the projection; they only
change prompt-time presence for entries already in the operator ceiling.
Boot-declared and operator-owned entries retain their existing shared
visibility and authorization gates. The session overlay contributes only its
narrow-only disable set.

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

The live-profile recovery request is only `{identity, agent_id}`. Its handler
uses the verified identity and signed agent reach, calls the existing
user-scope reconciler, and then reads the active user revision for the response.
It does not mint a JTI, accept a provider/descriptor/authority, or write a new
receipt. Existing removal and revision CAS fences therefore remain the sole
concurrency authority; a concurrent removal is reflected by the post-reconcile
fresh revision rather than a stale retry projection.

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
- [x] User server/tool loading-mode choices are validated as the closed
      `always`/`deferred` set, survive sibling writes, compose after the
      operator mode, and resolve logical names for physical user sources.
- [x] Generic user configuration writes carry the signed pair forward.
- [x] Physical MCP owner and exact teardown paths include the user identity;
      the same logical descriptor can map to distinct physical sources without
      a client-selected destination.
- [x] OAuth provider ownership remains private to the pair and does not widen
      the shared provider set or token destination.
- [x] Focused source tests, generator lockstep checks, static smoke, and the
      focused race gate pass locally.
- [x] Existing-pair retry calls the canonical live-profile reconcile method,
      uses its fresh projection, makes no new registration/JTI, and reports
      reconciliation failure as a truthful retryable error.

## Files added or changed

- Signed OAuth user service, scope authorization, replay identity, and
  reconciliation in `internal/runtime/agentcfg/protocol/`.
- User-aware physical MCP owner and source projection seams in
  `internal/tools/auth/`, `internal/tools/drivers/mcp/`, and
  `internal/runtime/serve/`.
- Canonical Protocol methods/types, stream routes, TypeScript client/types,
  and generated manifest/reference files.
- Two-user lifecycle and effective projection tests.
- D-448 and D-450, this phase plan, and `scripts/smoke/phase-265.sh`.

## Test plan

- `go test ./internal/runtime/agentcfg/protocol -run 'TestUserSignedOAuthMCPCapability'`.
- `go test ./internal/runtime/agentcfg/projection -run 'TestActivePlannerCatalogView_UserMCP'`.
- Focused `go test` for serve, MCP driver, stream, methods, and single-source
  packages.
- `go test -race` over the complete focused lifecycle/projection/serve/MCP
  set.
- User live-profile reconcile service/stream tests, including current-session
  identity, body/reach mismatch, two-user isolation, concurrent removal and
  fresh post-reconcile revision projection.
- User loading-mode validation, two-user differing-mode projection, and
  logical-to-physical personal-source loading tests.
- Protocol TypeScript, reference, and committed-file lockstep checks.
- Static phase smoke assertions and shell syntax validation.

## Smoke script additions

`scripts/smoke/phase-265.sh` asserts the user methods/types/routes, verified
authorization, `ConfigScopeUser`, full physical owner plumbing, user-only
projection, private OAuth provider ownership, the two-user tests, the live
profile reconcile route and test, D-448/D-450, and the generated contract
surfaces. It is static-only and does not claim a live runtime or hosted CI
result.

## Coverage target

Every new authorization, scope, owner, projection, route, and isolation branch
has a focused test. No unrelated package coverage claim is made.

## Risks and deliberate boundaries

- The registry remains process-global, but user-owned logical names are mapped
  to deterministic owner-derived physical source keys. Physical ids stay in
  the runtime registry/catalog only; desired pairs and exposure policy remain
  logical. A collision with an already-occupied derived key still fails loud.
- A pair is user desired state keyed by `(tenant, user, agent)`. The session
  that issued its signed authority remains audit/replay context, not durable
  attachment authority; a later session for the same user reconciles, uses,
  and removes the pair after restart.
- The physical registry is cache/materialization only. One shared effective
  source authorizer checks owner, logical pair membership, current user
  revision, and signed reach for list/get/resource/App/dispatch admission;
  stale physical entries do not become authority after a revision drop.
- This phase intentionally supports only the signed OAuth descriptor/envelope
  contract. Non-OAuth user attachment needs a separate closed descriptor and
  signer-authorized method; no empty-envelope or browser-selected fallback is
  permitted.
- Existing operator maintenance preserves its compatibility fence and skips
  user operation receipts; user reconciliation is keyed by the durable user
  attachment, while session identity is audit/replay context only.
- A runtime must expose this additive method before a consumer can perform an
  immediate retry after process-local attachment loss. Until that runtime
  contract is deployed, the caller reports live verification unavailable; no
  tag or compatibility version is inferred by this plan.

## Glossary additions

None.

## Public surface

- `AgentConfigUserRegisterOAuthMCPCapabilityRequest` / `Response`.
- `AgentConfigUserRemoveOAuthMCPCapabilityRequest` / `Response`.
- `AgentConfigUserReconcileLiveProfileRequest` / `Response`.
- `agent_config.user.register_oauth_mcp_capability`.
- `agent_config.user.remove_oauth_mcp_capability`.
- `agent_config.user.reconcile_live_profile`.
- `ProtocolClient.userRegisterOAuthMCPCapability` and
  `ProtocolClient.userRemoveOAuthMCPCapability` and
  `ProtocolClient.userReconcileLiveProfile`.

Protocol version remains `0.1.0`.

## Evidence status

Local focused tests, generated-source checks, the focused race gate, and the
static smoke pass at the implementation head. No hosted CI, release tag,
deployment, or downstream acceptance is claimed.

## Pre-merge checklist

- [x] Focused source, generated-surface, race, and Phase 265 smoke checks pass
      at the implementation head.
- [x] `make drift-audit` passes.
- [ ] `make preflight` passes — not run locally under the authorized scoped
      verification; hosted preflight remains the release gate.
- [x] `make check-mirror` passes.
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve.
- [x] Coverage target is met by focused tests for each stated authorization,
      scope, owner, projection, and isolation branch; no unrelated package
      coverage claim is made.
- [x] Multi-isolation paths: two-user and cross-session isolation tests pass.
- [x] Concurrent-reuse N/A — this phase adds no new compiled reusable
      artifact; concurrent lifecycle/projection behavior is covered by the
      focused isolation tests.
- [x] Integration test exists through the service, Protocol stream, serve,
      and projection seams, including identity and failure cases, under the
      focused race gate.
- [x] New vocabulary N/A — no glossary entry is required.
- [x] Brief departures N/A — no brief finding is departed from.
