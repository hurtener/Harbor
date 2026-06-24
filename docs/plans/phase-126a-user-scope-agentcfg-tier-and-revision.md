# Phase 126a — USER-scope agent-config tier + user-keyed store + the one durable user-scope write surface

## Summary

The agent-config control plane today has exactly two durable ownership
positions and one ephemeral one: the **admin/tenant** durable, versioned
config (keyed under a synthetic `__agentcfg__` user slot, so it is
agent-level) and the **session** safe-subset overlay (keyed by the real
triple, ephemeral, dies with the session, no diff/rollback). There is no
position in between — a non-admin caller cannot OWN a *durable, versioned*
config variant that spans their own sessions. This phase adds that missing
tier and makes it the **single durable user-scope WRITE surface for the
whole `126` band**: a closed-set USER authority scope (`agent_config:user`)
plus a user-keyed durable revision store (the caller's REAL `user_id`
becomes the revision key's isolation principal, never the synthetic
constant) and its in-phase consumer — a versioned `agent_config.user.*` verb
family (get / set_revision / list_revisions / diff / rollback) that
exercises the tier end-to-end with full diff/rollback parity.

A durable user revision is a **virtual agent**: one user-scope revision
carrying a personal instruction layer *and* a narrow-only tool subset,
written **atomically** through the one `set_revision` verb here.
Consequently the `AgentConfigUserPayload` this phase pins MUST carry BOTH
the **user prompt** (`user_prompt`) and the **narrow-only disables**
(`disabled_servers` / `disabled_tools`) as versioned fields, because the two
sibling phases that follow are being redesigned as **PROJECTION-ONLY**
consumers with NO write verbs of their own: Phase 126b projects
`user_prompt` into the run-start `<user_instructions>` composition, and
Phase 126c projects `disabled_servers` / `disabled_tools` into the run-start
narrow-only tool-exposure exclusion set. There is exactly one place a
durable user variant is written — `agent_config.user.set_revision` — and
every projection consumer reads the active revision back; no sibling grows a
private write hook.

The new tier is strictly below admin: its wire payload is the same
structurally-bounded safe subset the session overlay carries (user prompt
layer, narrow-only source/tool disables, ephemeral personal-skill
membership), so a USER caller still physically cannot widen a capability,
edit the operator base, or add an MCP connection — those stay admin-only and
fail-closed.

## RFC anchor

- RFC §5.5 — Authentication (the verified JWT scope a token carries; the
  closed extended-scope set the tier extends — RFC §5.5 enumerates the
  closed scope universe, so adding a scope is a Protocol-surface phase, not
  an ad-hoc string).
- RFC §6.16 — Agent Registry (the agent-config control plane's privilege
  tiers; `agent_id` is a key, never an isolation principal — the tuple
  stays `(tenant, user, session)`).
- RFC §6.3 — Steering and the unified pause/resume primitive (the
  verified-ctx authority model: authority derives from the verified
  context, never the request body).
- RFC §5.3 — Protocol versioning (bumping `ProtocolVersion` is an RFC
  change; this phase is additive — see "Protocol version" below).

## Briefs informing this phase

- brief 09
- brief 11

## Brief findings incorporated

- **brief 09 (agent-as-actor / scope):** privilege tiers are
  scope-claim-derived, not body-derived. This phase derives the USER tier
  from the verified ctx scope (`auth.ScopeAgentConfigUser` present → the
  user-scope verbs are admitted; absent → `scope_mismatch`, fail-closed),
  mirroring the verified-identity authority model the steering edge and the
  session safe-subset already prove correct. The agent-bound keying
  precedent (agent-bound OAuth tokens key by the registration `agent_id`)
  is reused: the user-scope revision keys by `(tenant, user)` with
  `agent_id` in the session slot, never as an isolation filter.
- **brief 11 (console feature surface):** the operator vs end-user surfaces
  are distinct; an end user gets a constrained, SAFE set of controls. This
  phase ships exactly that safe subset as a DURABLE, versioned per-user
  surface — the same shape the ephemeral session overlay carries (user
  prompt layer, narrow-only source toggles, ephemeral skills), promoted to
  a versioned revision chain a generic Protocol client (a third-party
  console, an IDE client, the SDK) can read back, diff, and roll back. The
  capability surface (base prompt, add/remove servers, allowlist widening,
  model swap) stays admin-only.

## Findings I'm departing from (if any)

None.

## Goals

- **Primitive (the new tier).** A new canonical, closed-set authority scope
  `auth.ScopeAgentConfigUser` (`"agent_config:user"`) added to
  `internal/protocol/auth/scopes.go` — minding the closed-set design
  (unknown scopes are dropped from the verified set, so an attacker cannot
  invent it). The scope-constant godoc enumeration in `scopes.go` is
  extended to name the new entitlement, with a note that RFC §5.5 fixes the
  closed scope universe.
- **Primitive (the user-keyed store).** A USER-scoped, durable, versioned
  revision store in the `agentcfg` registry: the same one implementation,
  parameterised by an `agentcfg.ConfigScope` discriminator
  (`ConfigScopeAgent` / `ConfigScopeUser` — named with the `ConfigScope`
  prefix precisely to disambiguate from the unrelated
  `tools/auth.BindingScope` constants `ScopeAgent` / `ScopeUser`, which are
  the OAuth token-binding kinds, and from the new Protocol JWT scope
  `auth.ScopeAgentConfigUser`). `ConfigScopeAgent` keys agent-level config
  under the existing synthetic identity (unchanged); `ConfigScopeUser` keys
  the per-user variant under the caller's REAL `(tenant, user)` with
  `agent_id` in the session slot. The isolation tuple is NOT widened — the
  real user is the isolation principal for the user variant, `agent_id`
  stays a key (RFC §6.16 / CLAUDE.md §6 clarifying note).
- **Namespace the two key spaces so they can NEVER alias (security).** The
  driver uses a DISTINCT record-kind PREFIX per `ConfigScope`:
  `ConfigScopeAgent` keeps `agentcfg.active` / `agentcfg.revision.<id>`;
  `ConfigScopeUser` writes `agentcfg.user.active` /
  `agentcfg.user.revision.<id>`. The distinct prefix is the structural
  guarantee that the two key spaces cannot collide REGARDLESS of identity
  values. As fail-loud defence-in-depth, a `ConfigScopeUser` call whose
  verified `user_id` equals the reserved `__agentcfg__` sentinel is REJECTED
  (`ErrReservedUser`) before any read or write — closing the latent
  privilege-escalation where `user_id == "__agentcfg__"` + the old shared
  `agentcfg.*` kind would have aliased byte-for-byte onto the agent-level
  admin chain (reads leaking the operator base config; writes tampering the
  agent-level active pointer for ALL users of that agent).
- **Consumer (the in-phase verb family).** A versioned `agent_config.user.*`
  Protocol family — get / set_revision / list_revisions / diff / rollback —
  that exercises the new tier and store end-to-end with diff/rollback parity
  to the admin registry verbs. Gated on `ScopeAgentConfigUser` (NOT admin),
  identity-mandatory, fail-loud. The set verb accepts only the
  structurally-bounded safe-subset payload (`AgentConfigUserPayload`: user
  prompt, narrow-only disables, personal-skill membership) — no base, no
  connections, no enable, no model swap — so widening is impossible at the
  wire, with the scope gate as defence-in-depth.
- **The one durable user-scope write surface for the band.** The
  `AgentConfigUserPayload` carries BOTH `user_prompt` (Phase 126b's
  projection source) and `disabled_servers` / `disabled_tools` (Phase 126c's
  projection source) plus `personal_skills`, written atomically by this one
  `set_revision` verb. Phases 126b/126c add NO write verb; they project the
  active user revision at run start. Pinning all projection-fed fields here
  is what lets the siblings stay projection-only.
- **Preserve the proven boundary.** The admin/user privilege split stays
  exactly as audited: adding a new MCP connection, editing the operator
  base, widening the tool allowlist, and swapping the model remain
  admin-only and fail-closed for a USER-scoped caller. The user write is
  audited (the author anchor records the real `(tenant, user)`).

## Non-goals

- **Run-start projection of the durable user variant.** Composing the
  active user-scope revision into a run's prompt layers (Phase 126b) and
  narrowed tool exposure (Phase 126c) at run start — layered between the
  admin config and the ephemeral session overlay — is the work of the two
  PROJECTION-ONLY sibling phases, which consume the fields this phase pins
  and add no write verb of their own. This phase ships the durable WRITE
  surface + the verb-family consumer; the run-start projection wiring (and
  its `cmd/harbor` + `harbortest/devstack` twin) is theirs. Tracked in Risks.
- **A new persistence subsystem.** The user variant is a revision in the
  SAME registry behind the SAME §9 StateStore triad — only the key's
  identity principal and record-kind prefix differ by scope.
- **Console rendering of the per-user surface.** A Console page for the
  per-user variant is a downstream `web/console` phase that consumes this
  Protocol surface; this phase ships the Runtime + Protocol surface only.
- **Minting the token.** The dev bootstrap already mints non-admin tokens
  with an explicit `scopes:[]` override (the non-admin token contract); a
  token carrying `["agent_config:user"]` needs no new minting code — the
  closed-set addition is enough for the verifier to retain the claim.
- **Widening any capability via the USER tier** — explicitly forbidden; the
  bounded payload shape and the scope gate both enforce it.
- **A `ProtocolVersion` bump.** The methods + wire types are additive — see
  "Protocol version" below.

## Protocol version

No `ProtocolVersion` bump. Per `internal/protocol/types/version.go`, a new
method, a new capability, and a new optional wire field are **Minor-class,
backward-compatible** additions (the `Version.Minor` godoc); a Major bump is
reserved for a breaking change. The five additive `agent_config.user.*`
methods and the additive `AgentConfigUserPayload` + request/response wire
types extend the surface without removing or re-typing any existing element,
so `ProtocolVersion` holds at `0.1.0`. RFC §5.3 governs ONLY the
trip-wire: *bumping* the pinned constant is an RFC change — which this phase
does not do.

## Acceptance criteria

- [ ] `auth.ScopeAgentConfigUser` (`"agent_config:user"`) is added to the
      closed canonical scope set; `IsValidScope` accepts it; `WithScopes`
      retains it from a verified token and an unknown scope is still
      dropped; `CanonicalScopes()` includes it. The `scopes.go` constant
      enumeration godoc names the new entitlement and notes the RFC §5.5
      closed scope universe.
- [ ] `agentcfg.ConfigScope` (`ConfigScopeAgent` / `ConfigScopeUser`)
      threads through the `agentcfg.Registry` methods; the StateStore driver
      keys `ConfigScopeAgent` under the existing synthetic identity AND the
      existing `agentcfg.active` / `agentcfg.revision.` kinds (byte-identical
      to today), and `ConfigScopeUser` under the caller's REAL
      `(tenant, user)` with `agent_id` in the session slot, the run zeroed,
      AND the DISTINCT `agentcfg.user.active` / `agentcfg.user.revision.`
      kinds. A golden test pins that `ConfigScopeAgent` keying + kinds are
      unchanged (no migration, no existing-data reshape).
- [ ] **The tree builds green after the `ConfigScope` signature change.**
      Adding the `scope` parameter to the `Registry` methods breaks every
      existing caller; ALL of them are migrated in this PR to pass
      `agentcfg.ConfigScopeAgent` (no behaviour change for the agent tier):
      `internal/runtime/agentcfg/projection/projection.go` (ActiveSkillViews,
      ActiveLLMOverrides, ActivePlannerCatalogView, ActivePromptLayers),
      `internal/runtime/agentcfg/protocol/{skills,addconnection,llmparams,mcppolicy,promptlayers}.go`,
      and `internal/mcpconsole/apps.go`. `make build` / `go build ./...` is
      green — no orphaned call site left on the old arity (the §17.6
      cross-cutting build-break is closed in the same PR, not deferred).
- [ ] **Key-space namespacing + sentinel rejection (security).** A
      `ConfigScopeUser` call whose verified `user_id == "__agentcfg__"` is
      rejected with `ErrReservedUser` before any read/write. A statestore
      test asserts a `user_id == "__agentcfg__"` `ConfigScopeUser` caller can
      NEITHER read the `ConfigScopeAgent` chain (no leak of the operator base
      config) NOR clobber its active pointer (the agent chain's bytes are
      unchanged after the rejected call) — proving the distinct kind prefix
      AND the sentinel rejection both hold.
- [ ] The five `agent_config.user.*` methods are registered once in
      `internal/protocol/methods` (constants + a
      `canonicalAgentConfigUserMethods` set + `IsAgentConfigUserMethod`, and
      added to `canonicalAgentConfigMethods`); the family-count godoc is
      bumped (`seventeen` → `twenty-two`). Their wire types are defined once
      in `internal/protocol/types/agentconfig.go` and registered in
      `internal/protocol/singlesource.CanonicalWireTypes`. No second
      definition site (CLAUDE.md §8).
- [ ] `cmd/harbor-gen-protocol-docs/methods.go` gains a join row per new
      verb (route + request/response type names), so the generated
      `docs/site/protocol/methods.md` lists them; `make protocol-docs-gen`
      regenerates and `make protocol-docs-gen-check` (`git diff --exit-code`)
      plus the generator lockstep test pass (a new method without its docs
      join row fails `go test`).
- [ ] The agent-config wire handler routes `user/get`, `user/set_revision`,
      `user/list_revisions`, `user/diff`, `user/rollback` and gates them on
      a verified `auth.ScopeAgentConfigUser` claim (the new middle tier):
      a caller WITHOUT the claim is rejected `scope_mismatch` (HTTP 403)
      before any dispatch; an ADMIN-only route is still rejected
      `scope_mismatch` for a user-only token; a session-safe route still
      needs no scope. Authority derives from the verified ctx, never the
      request body.
- [ ] A USER-scoped caller can `set_revision` → `list_revisions` →
      `diff` → `rollback` over their OWN durable variant: the set records an
      immutable, content-addressed revision under the real `(tenant, user)`
      key; list returns the chain newest-first; diff is the server-side
      compare; rollback repoints the user's active pointer WITHOUT mutating
      any revision. Idempotent re-set of byte-identical content is a no-op
      returning the existing revision (parity with the admin registry).
- [ ] The `AgentConfigUserPayload` input carries ONLY the safe subset and
      carries it COMPLETELY for the band: `user_prompt` (126b projects),
      `disabled_servers` + `disabled_tools` (126c projects), and
      `personal_skills`. It has no base / connections / enable / model field,
      so a USER caller physically cannot widen a capability or edit the
      operator base — a test asserts a forged extra field cannot reach a
      widening path.
- [ ] Cross-user isolation: user A's durable variant is invisible to user
      B — B's `user/get` returns `Set:false`, and B's `user/diff` /
      `user/rollback` against A's revision id fail loud with
      `ErrRevisionNotFound` (the id lives under A's key). A cross-user
      integration test asserts no bleed.
- [ ] The per-write lock is scope-aware: a `ConfigScopeUser` write serialises
      ONLY the same `(tenant, real-user, agent)` read-modify-write and NEVER
      serialises distinct users; a `ConfigScopeAgent` write serialises by
      `(tenant, agent)` exactly as today. The concurrency test asserts
      distinct-user `ConfigScopeUser` writes do not serialise.
- [ ] Identity-mandatory and fail-loud everywhere: an incomplete triple is
      rejected `identity_required`; a missing user-scope store fails loud
      (`501`/typed error), never a silent stub.
- [ ] The hand-maintained TS client mirrors the new wire types + methods and
      the wire manifest is regenerated (`make protocol-ts-gen`); the
      generated Protocol docs are regenerated (`make protocol-docs-gen`);
      `make protocol-ts-gen-check` and `make protocol-docs-gen-check` pass.
- [ ] `scripts/smoke/phase-126a.sh` is green (static + live skip-if-404).

## Files added or changed

```text
internal/protocol/auth/scopes.go                         # + ScopeAgentConfigUser (closed-set addition) + enumeration godoc + RFC §5.5 note
internal/protocol/auth/scopes_test.go                    # closed-set + retain/drop coverage
internal/protocol/methods/methods.go                     # + 5 user methods, canonicalAgentConfigUserMethods, IsAgentConfigUserMethod, count godoc 17->22
internal/protocol/methods/methods_test.go                # method-count + classifier coverage
internal/protocol/types/agentconfig.go                   # + AgentConfigUserPayload (user_prompt + disabled_servers/disabled_tools + personal_skills) + 5 req/resp pairs (responses reuse AgentConfigRevisionView / AgentConfigDiff)
internal/protocol/singlesource/singlesource.go           # register new wire types + methods
cmd/harbor-gen-protocol-docs/methods.go                  # + 5 join rows (route + req/resp type names) for the user verbs
docs/site/protocol/methods.md                            # regenerated via `make protocol-docs-gen`
docs/site/protocol/types.md                              # regenerated via `make protocol-docs-gen`
internal/protocol/transports/stream/agentconfig_handler.go  # + user/* routes + 3-tier scope gate
internal/protocol/transports/stream/agentconfig_handler_test.go
internal/agentcfg/agentcfg.go                            # + ConfigScope (ConfigScopeAgent/ConfigScopeUser); + ErrReservedUser; Registry methods gain a scope param
internal/agentcfg/drivers/statestore/statestore.go       # scope-aware keying (synthetic vs real-user) + DISTINCT kind prefix per scope + __agentcfg__ sentinel rejection
internal/agentcfg/drivers/statestore/statestore_test.go  # ScopeUser keying + golden ScopeAgent-unchanged + cross-user isolation + sentinel-collision rejection
internal/runtime/agentcfg/protocol/user.go               # NEW — UserGet/UserSetRevision/UserListRevisions/UserDiff/UserRollback verbs + payload mapping
internal/runtime/agentcfg/protocol/user_test.go          # NEW
internal/runtime/agentcfg/protocol/service.go            # scope-aware write-lock key; existing admin call sites pass ConfigScopeAgent
internal/runtime/agentcfg/protocol/concurrency_test.go   # extend: N>=100 mixed admin/user-scope writes under -race + distinct-user non-serialisation assertion
internal/runtime/agentcfg/protocol/siblings_matrix_test.go  # extend tier matrix with the user tier
# --- §17.6 MUST-FIX: the Registry.Active / Registry.SetRevision (and sibling) signatures gain the ConfigScope param,
# --- which breaks 10 PRODUCTION call sites. ALL of them are migrated in THIS PR to pass agentcfg.ConfigScopeAgent
# --- (no behaviour change for the agent tier). The tree does not build until every one is updated:
internal/runtime/agentcfg/projection/projection.go       # :80 ActiveSkillViews, :138 ActiveLLMOverrides, :200 ActivePlannerCatalogView, :250 ActivePromptLayers — each .Active(...) call passes agentcfg.ConfigScopeAgent
internal/runtime/agentcfg/protocol/skills.go             # :141 s.registry.Active(...) — pass agentcfg.ConfigScopeAgent
internal/runtime/agentcfg/protocol/addconnection.go      # :289 s.registry.Active(...) — pass agentcfg.ConfigScopeAgent
internal/runtime/agentcfg/protocol/llmparams.go          # :55 s.registry.Active(...) — pass agentcfg.ConfigScopeAgent
internal/runtime/agentcfg/protocol/mcppolicy.go          # :46 s.registry.Active(...) — pass agentcfg.ConfigScopeAgent
internal/runtime/agentcfg/protocol/promptlayers.go       # :48 s.registry.Active(...) — pass agentcfg.ConfigScopeAgent
internal/mcpconsole/apps.go                              # :316 a.agentCfg.Active(...) — pass agentcfg.ConfigScopeAgent
web/console/src/lib/protocol/agentconfig.ts              # hand-mirror new wire types
web/console/src/lib/protocol/client.ts                   # typed calls for the 5 user methods
web/console/src/lib/protocol/wire-manifest.gen.json      # regenerated via `make protocol-ts-gen`
test/integration/agentcfg_user_scope_test.go             # NEW — real registry+bus+handler, admin/user/session contexts, cross-user isolation, sentinel rejection, -race
scripts/smoke/phase-126a.sh                              # NEW
docs/plans/phase-126a-user-scope-agentcfg-tier-and-revision.md
docs/decisions.md                                        # D-256
docs/glossary.md                                         # "user-scope agent-config tier", "durable user config variant"
docs/plans/README.md                                     # 126a row Pending (V1.6) + detail-block stub
README.md                                                # Status table 126a row (if it surfaces a reader-facing surface)
```

No new top-level directory; AGENTS.md §3 unchanged.

## Public API surface

```go
// internal/protocol/auth — the new Protocol JWT scope (closed-set addition).
const ScopeAgentConfigUser Scope = "agent_config:user"

// internal/agentcfg — the durable-config ownership discriminator.
// Named with the ConfigScope* prefix to disambiguate from the unrelated
// tools/auth.BindingScope constants (ScopeAgent/ScopeUser = OAuth token
// binding) and from the Protocol JWT scope auth.ScopeAgentConfigUser.
type ConfigScope uint8
const (
    ConfigScopeAgent ConfigScope = iota // admin/tenant durable config (synthetic key, agentcfg.* kinds) — the default
    ConfigScopeUser                     // per-user durable config variant (real-user key, agentcfg.user.* kinds)
)

// ErrReservedUser — a ConfigScopeUser call carried a verified user_id equal
// to the reserved internal sentinel ("__agentcfg__"); fails closed so it can
// never alias onto the agent-level chain.
var ErrReservedUser = errors.New("agentcfg: user id collides with the reserved internal slot")

// Registry methods gain a scope parameter (one implementation, two keyings):
type Registry interface {
    SetRevision(ctx context.Context, id identity.Quadruple, agentID string, scope ConfigScope, payload ConfigPayload) (Revision, error)
    Active(ctx context.Context, id identity.Quadruple, agentID string, scope ConfigScope) (Revision, bool, error)
    Get(ctx context.Context, id identity.Quadruple, agentID, revisionID string, scope ConfigScope) (Revision, error)
    ListRevisions(ctx context.Context, id identity.Quadruple, agentID string, scope ConfigScope, limit int) ([]Revision, error)
    Rollback(ctx context.Context, id identity.Quadruple, agentID, revisionID string, scope ConfigScope) (Revision, error)
    Diff(ctx context.Context, id identity.Quadruple, agentID, fromRev, toRev string, scope ConfigScope) (Diff, error)
    Close(ctx context.Context) error
}

// internal/runtime/agentcfg/protocol — the user-scope verb family (the consumer).
func (s *Service) UserGet(ctx context.Context, req prototypes.AgentConfigUserGetRequest) (prototypes.AgentConfigUserGetResponse, error)
func (s *Service) UserSetRevision(ctx context.Context, req prototypes.AgentConfigUserSetRevisionRequest) (prototypes.AgentConfigUserSetRevisionResponse, error)
func (s *Service) UserListRevisions(ctx context.Context, req prototypes.AgentConfigUserListRevisionsRequest) (prototypes.AgentConfigUserListRevisionsResponse, error)
func (s *Service) UserDiff(ctx context.Context, req prototypes.AgentConfigUserDiffRequest) (prototypes.AgentConfigUserDiffResponse, error)
func (s *Service) UserRollback(ctx context.Context, req prototypes.AgentConfigUserRollbackRequest) (prototypes.AgentConfigUserRollbackResponse, error)
```

### The canonical StateStore keying scheme (PINNED — 126b/126c reference this verbatim)

This is the one keying scheme the whole `126` band keys around. The two
`ConfigScope` arms are namespaced on TWO independent dimensions — the
identity slot occupant AND the record-kind prefix — so they can never alias:

| dimension                | `ConfigScopeAgent` (admin/tenant) | `ConfigScopeUser` (per-user variant) |
|--------------------------|-----------------------------------|--------------------------------------|
| `Identity.TenantID`      | caller's verified tenant          | caller's verified tenant             |
| `Identity.UserID`        | synthetic `"__agentcfg__"`        | caller's REAL verified `user_id`     |
| `Identity.SessionID`     | `agent_id` (the per-agent key)    | `agent_id` (the per-agent key)       |
| run component            | zeroed                            | zeroed                               |
| active-pointer kind      | `agentcfg.active`                 | `agentcfg.user.active`               |
| revision kind prefix     | `agentcfg.revision.<id>`          | `agentcfg.user.revision.<id>`        |
| reserved-sentinel guard  | (n/a — synthetic is intentional)  | reject `user_id == "__agentcfg__"` (`ErrReservedUser`) |

`agent_id` lives in the `SessionID` slot for BOTH scopes (it is a key, never
an isolation filter — RFC §6.16). The distinct record-kind prefix is the
structural guarantee that the two key spaces cannot collide regardless of
identity values; the `__agentcfg__` sentinel rejection on the `ConfigScopeUser`
arm is fail-loud defence-in-depth. `ConfigScopeUser`'s `ListRevisions` filters
the maintenance scan to the real `(tenant, user, agent)` slot AND the
`agentcfg.user.revision.` prefix.

### The scope-aware write-lock key (PINNED)

The Service is the sole registry-writer; it serialises each
read-modify-write under a per-owner lock whose key now includes the scope so
the two tiers never contend across each other and distinct users never
contend within the user tier:

- `ConfigScopeAgent`: key `(ConfigScopeAgent, tenant, agent_id)` — unchanged
  agent-level serialisation.
- `ConfigScopeUser`: key `(ConfigScopeUser, tenant, real-user, agent_id)` —
  same-owner read-modify-write serialised; distinct users NEVER serialised.

Wire types (single source, `internal/protocol/types/agentconfig.go`):
`AgentConfigUserPayload` (the bounded safe-subset input, mirroring the
session overlay's field set) + the five request/response pairs. The payload
carries every field a band projection consumes:

```go
// AgentConfigUserPayload is the bounded safe-subset a user-scope revision
// persists — the ONE durable user write surface for the band. It mirrors the
// session overlay's field set (user prompt + narrow-only disables + personal
// skills) and has NO base / connections / enable / model field.
type AgentConfigUserPayload struct {
    UserPrompt      string   `json:"user_prompt,omitempty"`       // the prompt projection consumes this
    DisabledServers []string `json:"disabled_servers,omitempty"`  // the tool-exposure projection consumes this
    DisabledTools   []string `json:"disabled_tools,omitempty"`    // the tool-exposure projection consumes this
    PersonalSkills  []string `json:"personal_skills,omitempty"`
}
```

The verb maps `UserPrompt` onto `ConfigPayload.PromptLayers.User`,
`DisabledServers`/`DisabledTools` onto `ConfigPayload.ToolExposure`
(`PausedServers`/`DisabledTools`), and `PersonalSkills` onto
`ConfigPayload.Skills.Names`; `Base`, `Connections`, and `LLM` stay nil — so
the restricted mapping is the structural widening boundary. Responses REUSE
the canonical `AgentConfigRevisionView` (payload sections the user tier never
sets stay nil) and `AgentConfigDiff` (base/connections/llm sub-diffs are
always no-change for the user tier) — giving literal diff/rollback parity
with the admin registry surface and minimising new manifest entries.

## Test plan

- **Unit:**
  - `auth`: the closed set includes `ScopeAgentConfigUser`; `WithScopes`
    retains it and still drops an unknown scope; `HasScope` gates it.
  - `methods`: `IsAgentConfigUserMethod` true for the five user verbs and
    false for admin / session verbs; the family count assertion updated to
    twenty-two.
  - `statestore`: `ConfigScopeUser` keys under the real `(tenant, user)` +
    `agent_id`-in-session AND the `agentcfg.user.*` kinds; `ConfigScopeAgent`
    keying + kinds byte-identical to today (golden); a user revision is
    invisible across users and across tenants;
    `ListRevisions(ConfigScopeUser)` filters to the user slot + user kind;
    idempotent re-set; **the sentinel-collision test** — a
    `user_id == "__agentcfg__"` `ConfigScopeUser` call is rejected
    (`ErrReservedUser`) and the `ConfigScopeAgent` chain is neither read nor
    clobbered.
  - `runtime/agentcfg/protocol`: the user verbs validate identity, map the
    bounded payload onto the restricted `ConfigPayload`, reject a missing
    store loud; the set→diff→rollback round-trip; a widening attempt has no
    reachable path (shape + mapping); the scope-aware lock key is exercised.
  - handler: the three-tier route gate — session-safe (no scope), user
    (`ScopeAgentConfigUser`), admin (`ScopeAdmin`); a user-only token is
    403 on an admin route; an admin route caller without the user scope is
    403 on a user route; an incomplete triple is 401.
- **Integration:** `test/integration/agentcfg_user_scope_test.go` — REAL
  registry (StateStore in-mem driver) + REAL bus + REAL wire handler with
  three verified contexts (admin, user-scoped, plain session). Admin sets
  agent-level config; user A sets a durable user variant
  (set→list→diff→rollback); user B cannot see or roll back A's revision
  (cross-user isolation); a plain-session token is rejected on the user
  route (scope gate); a `user_id == "__agentcfg__"` user-scope caller is
  rejected and the admin chain is untouched (sentinel); identity propagation
  asserted through the edge; a widening attempt (forged body field) reaches
  no widening path. Runs under `-race`.
- **Conformance:** the existing `agentcfg` driver conformance suite is
  extended to run its revision/diff/rollback matrix under BOTH
  `ConfigScopeAgent` and `ConfigScopeUser`, asserting parity and cross-scope
  invisibility (an agent-scope revision id is not resolvable under the user
  scope and vice-versa — the distinct kind prefix guarantees this).
- **Concurrency / leak:** `internal/runtime/agentcfg/protocol/concurrency_test.go`
  extended — N≥100 concurrent invocations against a SINGLE shared Service +
  Registry, mixing admin-scope and user-scope writes across distinct users
  and the SAME user, under `-race`: no data races, no context bleed (run
  A's identity never keys run B), no cross-cancellation, no goroutine leak
  (baseline restored). The scope-aware per-owner write lock is asserted to
  serialise same-owner read-modify-write while NEVER serialising distinct
  users (and never serialising across the agent/user tiers).

## Smoke script additions

`scripts/smoke/phase-126a.sh` (mirrors the phase-92g static+live shape):

- Static — the new scope constant:
  `assert_grep_present 'ScopeAgentConfigUser Scope = "agent_config:user"' internal/protocol/auth/scopes.go`.
- Static — the closed-set membership: `ScopeAgentConfigUser:` present in the
  `canonicalScopes` map.
- Static — the five user methods:
  `MethodAgentConfigUserSetRevision Method = "agent_config.user.set_revision"`
  (and the other four), plus `func IsAgentConfigUserMethod`.
- Static — the user tier classifier set `canonicalAgentConfigUserMethods`.
- Static — the `agentcfg.ConfigScope` discriminator + the user keying +
  namespacing: `ConfigScopeUser` constant, the driver's `agentcfg.user.`
  kind prefix, and the `ErrReservedUser` sentinel rejection.
- Static — the bounded payload carries the band's projection fields:
  `disabled_servers` and `user_prompt` on `AgentConfigUserPayload`.
- Static — the Service verbs: `func (s *Service) UserSetRevision`,
  `UserListRevisions`, `UserDiff`, `UserRollback`, `UserGet`.
- Static — the handler routes + tier gate: `agentConfigUserRoutes` set and
  the `user/set_revision` route arm.
- Static — the generated-docs join rows: the user methods in
  `cmd/harbor-gen-protocol-docs/methods.go` and the method rows in
  `docs/site/protocol/methods.md`.
- Static — TS mirror + manifest: the user methods in
  `web/console/src/lib/protocol/client.ts`, `AgentConfigUserPayload` in
  `agentconfig.ts`, and the manifest covers `AgentConfigUserSetRevisionRequest`.
- Live (skip-if-404 per the SKIP convention). Requires three dev-minted
  tokens, named consistently across the block: `USER_TOKEN` (carries
  `agent_config:user`), `ADMIN_TOKEN` (carries `admin`), and `NOSCOPE_TOKEN`
  (a valid identity with NO extended scope). A token carrying
  `agent_config:user` `POST /v1/agent_config/user/set_revision` → 200 and a
  follow-up `user/list_revisions` returns the revision; the SAME `USER_TOKEN`
  on `POST /v1/agent_config/set_revision` (admin route) → 403
  `scope_mismatch`; `NOSCOPE_TOKEN` on `user/set_revision` → 403
  `scope_mismatch`. Skips cleanly on a build without the user-scope store
  wired.

## Coverage target

- `internal/protocol/auth`: ≥ 85%
- `internal/agentcfg`: ≥ 85%
- `internal/agentcfg/drivers/statestore`: ≥ 85%
- `internal/runtime/agentcfg/protocol`: ≥ 85%
- `internal/protocol/transports/stream` (agent-config handler): maintain ≥ 85%

## Dependencies

- Phase 92a — agent-config registry + revisions (the primitive this tier
  extends; the `Registry` interface + StateStore driver + diff/rollback).
- Phase 92g — session-user safe subset (the safe-subset shape + the
  verified-ctx tier gate + the projection-narrowing precedent this reuses;
  `AgentConfigSessionOverlay` is the field-set this payload mirrors).
- Phase 116 — non-admin token contract (mints the lesser-privileged token
  the new scope rides on; the `scopes:[]` dev-mint override).
- Phase 61 — Protocol auth (the verified-scope middleware that retains the
  closed-set scope and makes `auth.HasScope` load-bearing at the edge).

## Risks / open questions

- **The durable user variant has no run-start effect until its projection
  siblings land.** This phase's in-phase consumer is the versioned verb
  family (which exercises the store end-to-end with a test, satisfying the
  primitive-with-consumer rule exactly as the admin registry shipped before
  its projection consumers). The load-bearing run-start projections are
  Phases 126b (`user_prompt` → `<user_instructions>`) and 126c
  (`disabled_servers`/`disabled_tools` → narrow-only exclusion set), which
  are PROJECTION-ONLY (no write verb) and consume the fields this phase
  pins. Because all projection-fed fields are pinned in
  `AgentConfigUserPayload` here, the siblings need no schema change — they
  read the active revision and project. They carry the `cmd/harbor` +
  `harbortest/devstack` projection twin per §17.6.
- **Key-space aliasing was a real latent escalation.** Before this phase a
  `ConfigScopeUser` write keyed by the real `(tenant, user)` with a
  shared `agentcfg.*` kind would have aliased the agent-level admin chain
  byte-for-byte the instant a user's verified `user_id` happened to be
  `"__agentcfg__"`. The distinct kind prefix closes it structurally
  (regardless of identity values) and the sentinel rejection closes it
  loudly; both ship with a test. This is why the namespacing is an
  acceptance criterion, not an implementation detail.
- **Interface-signature churn is a cross-cutting build-break.** Adding the
  `ConfigScope` parameter to the `Registry` methods breaks every existing
  caller — not just the Service + tests + the conformance suite, but the
  run-start projection (`internal/runtime/agentcfg/projection/projection.go`,
  four `.Active(...)` sites), the sibling protocol verbs
  (`skills.go`/`addconnection.go`/`llmparams.go`/`mcppolicy.go`/`promptlayers.go`),
  and the MCP-console apps surface (`internal/mcpconsole/apps.go`). ALL of
  them are migrated in THIS PR to pass `agentcfg.ConfigScopeAgent` (the §17.6
  "fix what the build-break finds, in the same PR" rule — a build-break is
  never deferred). This is mechanical (existing callers keep agent-tier
  behaviour) and is chosen deliberately over a parallel user-scope method
  set, which would be a §13 "two parallel implementations" smell — one
  implementation, two keyings is the correct shape. The golden test pins
  that `ConfigScopeAgent` behaviour is byte-identical so the change is
  provably non-breaking for existing data.
- **Scope / discriminator naming.** `agent_config:user` follows the
  `console:fleet` namespaced convention; a bare `user` was rejected (it
  would read as a generic role). The store discriminator is named
  `ConfigScopeAgent` / `ConfigScopeUser` specifically to NOT collide in a
  reader's head with `tools/auth.BindingScope`'s `ScopeAgent` / `ScopeUser`
  (OAuth token binding) or the Protocol JWT scope `auth.ScopeAgentConfigUser`
  — three distinct concepts that would otherwise share a bare `ScopeUser`
  spelling.
- **Admin vs user scope orthogonality.** The user verbs gate on
  `ScopeAgentConfigUser` specifically, NOT admin — an admin token does not
  implicitly own per-user variants (admin owns the agent-level config). A
  caller that needs both surfaces carries both claims. This keeps the
  boundary crisp and testable; documented in D-256.
- Full §16 brief pass (brief 09 + brief 11 + RFC §5.5 / §6.16 / §6.3 / §5.3)
  when dispatched.

## Glossary additions

- **user-scope agent-config tier** — the middle tier of the agent-config
  authorization matrix (D-256): a non-admin caller carrying the
  `agent_config:user` scope owns a DURABLE, VERSIONED safe-subset config
  variant that spans their own sessions — distinct from the admin/tenant
  durable config (above it) and the ephemeral session overlay (below it).
- **durable user config variant** — the per-user revision chain the
  user-scope tier owns: keyed by the caller's real `(tenant, user)` with
  `agent_id` as the per-agent key under a distinct `agentcfg.user.*` record
  kind, carrying the band-complete safe subset (user prompt layer,
  narrow-only source/tool disables, personal-skill membership), with full
  diff/rollback. The one durable user-scope write surface; its projection
  consumers (the run-start prompt layer + tool-exposure overlay) read it
  back. A single such revision is a "virtual agent": a personal instruction
  layer + a narrow-only tool subset written atomically.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes (no AGENTS.md/CLAUDE.md edits expected)
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] `make build` / `go build ./...` is green — all 10 production call
      sites of the now-scope-parameterised `Registry` methods are migrated
      to pass `agentcfg.ConfigScopeAgent` (projection ×4, the five sibling
      protocol verbs, mcpconsole apps); no orphaned old-arity caller remains
- [ ] If multi-isolation paths changed: cross-session/cross-user isolation
      test passes (the user-keyed store is a new isolation principal; the
      cross-user invisibility test AND the sentinel-collision rejection test
      are mandatory)
- [ ] **This phase builds a reusable artifact (the scope-aware Registry +
      Service): concurrent-reuse test passes — N≥100 concurrent invocations
      against a single shared instance under `-race`, asserting no data
      races, no context bleed, no cancellation cross-talk, no goroutine
      leaks, and distinct-user non-serialisation of the scope-aware lock.**
- [ ] **This phase consumes shipped subsystems (92a registry, 61 auth) and
      closes a cross-subsystem seam (auth scope → handler tier → registry
      keying): `test/integration/agentcfg_user_scope_test.go` wires real
      drivers end-to-end, asserts identity propagation, covers ≥1 failure
      mode (scope gate + cross-user invisibility + sentinel collision), runs
      under `-race`.**
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md
      entry filed — N/A, no departures (D-256 records the design)
- [ ] TS client mirrored + `make protocol-ts-gen` manifest regenerated +
      `make protocol-ts-gen-check` green
- [ ] Generated Protocol docs regenerated (`make protocol-docs-gen`) +
      `make protocol-docs-gen-check` green (the new `cmd/harbor-gen-protocol-docs`
      join rows land in the same PR); `docs/site` nav unaffected (no new
      skill/recipe page)

---

## Implementation handoff

### (a) Master-plan `docs/plans/README.md` index row

Insert into the agent-config band of the index table (column format
`| #  | Name | Subsystem | RFC § | Deps | Cov. | Status |`):

```text
|126a| USER-scope agent-config tier: durable per-user config variant + user-keyed revision store + the one durable user-scope write verb (set/list/diff/rollback) | agentcfg | §5.5, §6.16, §6.3 | 92a, 92g, 116, 61 | 85% | Pending (V1.6) |
```

### (b) `docs/decisions.md` entry (markdownlint-clean — blank lines around the heading and the rules)

```markdown

---

## D-256 — A durable USER-scope agent-config tier sits between the admin/tenant durable config and the ephemeral session overlay, and is the band's one durable user write surface

**Context.** The agent-config control plane had exactly two durable
ownership positions and one ephemeral one. Admin/tenant durable config is
keyed under a synthetic `__agentcfg__` user slot (so it is agent-level and
two non-admin users of the same agent share one slot); the only non-admin
write path is the session overlay, keyed by the full real triple, ephemeral
(dies with the session), with no versioning, diff, or rollback. The
canonical scope set was binary (`admin`, `console:fleet`) — there was no
user tier. A non-admin caller therefore could not own a durable, versioned
config variant spanning their own sessions, which a richer generic Protocol
client (a third-party console, an IDE client, the SDK) needs.

**Decision.** Introduce a durable USER-scope tier and make it the ONE
durable user-scope write surface for the agent-config band:

1. A new closed-set authority scope `auth.ScopeAgentConfigUser`
   (`"agent_config:user"`). It is the durable-agent-config-ownership
   entitlement; unknown scopes stay dropped, so it cannot be forged (RFC
   §5.5 fixes the closed scope universe). It is strictly below admin and
   orthogonal to it — the user verbs gate on this scope specifically, not on
   admin.
2. An `agentcfg.ConfigScope` discriminator (`ConfigScopeAgent` /
   `ConfigScopeUser`) threaded through the `agentcfg.Registry` methods. ONE
   implementation, two keyings: `ConfigScopeAgent` keeps the synthetic
   agent-level keying AND the existing `agentcfg.*` record kinds
   (byte-identical to before — no migration); `ConfigScopeUser` keys the
   variant under the caller's REAL `(tenant, user)` with `agent_id` in the
   session slot, the run zeroed, AND a DISTINCT `agentcfg.user.*` record-kind
   prefix. The distinct kind prefix is the structural guarantee the two key
   spaces can never alias regardless of identity values; a `ConfigScopeUser`
   call whose verified `user_id` equals the reserved `__agentcfg__` sentinel
   is REJECTED (`ErrReservedUser`) as fail-loud defence-in-depth — closing a
   latent privilege escalation where that identity value would have aliased
   the agent-level admin chain. The discriminator is named with the
   `ConfigScope` prefix to disambiguate from `tools/auth.BindingScope`
   (`ScopeAgent`/`ScopeUser` = OAuth binding) and from
   `auth.ScopeAgentConfigUser` (the Protocol JWT scope). The isolation tuple
   is NOT widened: the real user is the isolation principal for the user
   variant, `agent_id` stays a key, never a `WHERE`-clause isolation filter
   (RFC §6.16 / §6 clarifying note). Threading the `scope` parameter through
   the interface (rather than a parallel user-scope method set) breaks every
   existing caller; all are migrated in the same PR to pass
   `ConfigScopeAgent` — the projection (×4), the sibling protocol verbs, and
   the MCP-console apps surface — so the tree builds green.
3. A versioned `agent_config.user.*` verb family (get / set_revision /
   list_revisions / diff / rollback) as the in-phase consumer, with full
   diff/rollback parity to the admin registry verbs. Its input is a
   structurally-bounded safe-subset payload (`AgentConfigUserPayload`) that
   carries the BAND-COMPLETE field set — `user_prompt`, `disabled_servers`,
   `disabled_tools`, `personal_skills` — and NO base / connections / enable /
   model field, so a USER caller physically cannot widen a capability or edit
   the operator base; the verified-ctx scope gate is defence-in-depth. The
   per-owner write lock is scope-aware (`ConfigScopeUser` keys by
   `(scope, tenant, real-user, agent)` so distinct users never serialise;
   `ConfigScopeAgent` keys by `(scope, tenant, agent)`). Adding an MCP
   connection, editing the operator base, widening the allowlist, and
   swapping the model stay admin-only and fail-closed.

**Consequences.** This is the band's single durable user write surface: a
user revision is a "virtual agent" (a personal instruction layer + a
narrow-only tool subset written atomically). The two sibling phases that
follow are PROJECTION-ONLY — Phase 126b projects `user_prompt` into the
run-start `<user_instructions>` composition and Phase 126c projects
`disabled_servers`/`disabled_tools` into the narrow-only tool-exposure
exclusion set; neither adds a write verb, because every projection-fed field
is pinned in `AgentConfigUserPayload` here. Authority derives from the
verified ctx, never the request body (consistent with the steering edge and
the session safe subset). The user write is audited under the real
`(tenant, user)` author anchor. The interface gains a `scope` parameter
rather than a parallel method set (avoiding a §13 two-implementations smell);
existing admin call sites pass `ConfigScopeAgent`. The methods + wire types
are additive (Minor-class per `internal/protocol/types/version.go`) — no
`ProtocolVersion` bump.

```

### (c) `scripts/smoke/phase-126a.sh` assertions to add

```bash
# Static — the new closed-set scope.
assert_grep_present 'ScopeAgentConfigUser Scope = "agent_config:user"' \
  internal/protocol/auth/scopes.go "user-scope authority scope constant"
assert_grep_present 'ScopeAgentConfigUser:' \
  internal/protocol/auth/scopes.go "user scope is in the closed canonicalScopes set"

# Static — the five user methods + the tier classifier.
assert_grep_present 'MethodAgentConfigUserSetRevision Method = "agent_config.user.set_revision"' \
  internal/protocol/methods/methods.go "user set_revision method"
assert_grep_present 'MethodAgentConfigUserRollback Method = "agent_config.user.rollback"' \
  internal/protocol/methods/methods.go "user rollback method"
assert_grep_present 'func IsAgentConfigUserMethod' \
  internal/protocol/methods/methods.go "user tier method classifier"
assert_grep_present 'canonicalAgentConfigUserMethods' \
  internal/protocol/methods/methods.go "user tier method set"

# Static — the durable-config scope discriminator + the namespaced keying.
assert_grep_present 'ConfigScopeUser' \
  internal/agentcfg/agentcfg.go "ConfigScope user-scope discriminator"
assert_grep_present 'agentcfg.user.' \
  internal/agentcfg/drivers/statestore/statestore.go "distinct user record-kind prefix"
assert_grep_present 'ErrReservedUser' \
  internal/agentcfg/agentcfg.go "reserved-sentinel rejection error"

# Static — the bounded payload carries the band's projection-fed fields.
assert_grep_present 'user_prompt' \
  internal/protocol/types/agentconfig.go "AgentConfigUserPayload carries the prompt-projection field"
assert_grep_present 'disabled_servers' \
  internal/protocol/types/agentconfig.go "AgentConfigUserPayload carries the tool-exposure disable field"

# Static — the Service verbs (the consumer).
assert_grep_present 'func (s \*Service) UserSetRevision' \
  internal/runtime/agentcfg/protocol/user.go "user set_revision verb"
assert_grep_present 'func (s \*Service) UserRollback' \
  internal/runtime/agentcfg/protocol/user.go "user rollback verb"

# Static — the handler route set + the user tier gate.
assert_grep_present 'agentConfigUserRoutes' \
  internal/protocol/transports/stream/agentconfig_handler.go "user route tier set"
assert_grep_present '"user/set_revision"' \
  internal/protocol/transports/stream/agentconfig_handler.go "user set_revision route"

# Static — generated-docs join rows + TS mirror + manifest.
assert_grep_present 'MethodAgentConfigUserSetRevision' \
  cmd/harbor-gen-protocol-docs/methods.go "generated-docs join row for the user verb"
assert_grep_present 'agent_config.user.set_revision' \
  docs/site/protocol/methods.md "generated docs row for the user verb"
assert_grep_present 'AgentConfigUserPayload' \
  web/console/src/lib/protocol/agentconfig.ts "TS mirror of the bounded user payload"
assert_grep_present 'AgentConfigUserSetRevisionRequest' \
  web/console/src/lib/protocol/wire-manifest.gen.json "manifest covers the user request type"

# Live (skip-if-404). Requires three dev-minted tokens named consistently:
#   USER_TOKEN     — carries agent_config:user
#   ADMIN_TOKEN    — carries admin
#   NOSCOPE_TOKEN  — a valid identity with no extended scope
# Skips cleanly when the user-scope store is not wired or the route 404s.
# 1. user-scope caller can write its durable variant.
assert_post_status "$(api_url /v1/agent_config/user/set_revision)" 200 \
  '{"identity":{...},"agent_id":"a1","payload":{"user_prompt":"be terse","disabled_servers":["weather"]}}' \
  "user-scope set_revision succeeds for an agent_config:user token" \
  --header "Authorization: Bearer ${USER_TOKEN}" --skip-on-404
# 2. same token is rejected on an ADMIN route (no admin claim).
assert_post_status "$(api_url /v1/agent_config/set_revision)" 403 \
  '{"identity":{...},"agent_id":"a1","payload":{}}' \
  "user-scope token is scope_mismatch on the admin set_revision route" \
  --header "Authorization: Bearer ${USER_TOKEN}" --skip-on-404
# 3. a token WITHOUT the user scope is rejected on the user route.
assert_post_status "$(api_url /v1/agent_config/user/set_revision)" 403 \
  '{"identity":{...},"agent_id":"a1","payload":{"user_prompt":"x"}}' \
  "no-scope token is scope_mismatch on the user route" \
  --header "Authorization: Bearer ${NOSCOPE_TOKEN}" --skip-on-404
```

`assert_grep_present` and `assert_post_status` already live in
`scripts/smoke/common.sh`. If a `--skip-on-404` flag does not yet exist on
`assert_post_status`, add it to `common.sh` with a one-line docstring
(`# assert_post_status URL CODE BODY MSG [--header H] [--skip-on-404]: POST and
assert status, treating a 404 as SKIP per the sacred convention`) OR gate the
live block with a `skip_if_404` probe on `/v1/agent_config/user/set_revision`
per the 404/405/501 → SKIP convention.

### (d) Master-plan per-phase detail-block stub

Append to the agent-config band detail prose in `docs/plans/README.md`:

> **Phase 126a — USER-scope agent-config tier (D-256).** Adds the missing
> middle tier of the agent-config authorization matrix and the band's ONE
> durable user-scope write surface: a durable, versioned per-user config
> variant that spans a non-admin caller's own sessions, sitting between the
> admin/tenant durable config (above) and the ephemeral session overlay
> (below). Three pieces land together (primitive + consumer per §13): (1) a
> new closed-set authority scope `agent_config:user` in
> `internal/protocol/auth/scopes.go`; (2) a user-keyed durable revision
> store — the `agentcfg.Registry` gains an `agentcfg.ConfigScope`
> discriminator so the one implementation keys agent-level config under the
> existing synthetic slot (`ConfigScopeAgent`, `agentcfg.*` kinds) and the
> per-user variant under the caller's REAL `(tenant, user)` with `agent_id`
> in the session slot (`ConfigScopeUser`, DISTINCT `agentcfg.user.*` kinds +
> a `__agentcfg__` sentinel rejection so the two key spaces can never alias),
> never widening the isolation tuple; and (3) the in-phase consumer — a
> versioned `agent_config.user.*` verb family
> (get/set_revision/list_revisions/diff/rollback) gated on the new scope,
> with diff/rollback parity to the admin registry and a structurally-bounded
> safe-subset payload (`AgentConfigUserPayload`) carrying the band-complete
> field set (`user_prompt` + `disabled_servers`/`disabled_tools` +
> `personal_skills`) so a user caller cannot widen capabilities or edit the
> operator base. The `ConfigScope` parameter breaks all existing `Registry`
> callers (projection ×4, the sibling protocol verbs, mcpconsole apps); all
> are migrated to pass `ConfigScopeAgent` in the same PR so the tree builds
> green. The two sibling phases are PROJECTION-ONLY consumers of this payload
> (126b projects `user_prompt`; 126c projects the disable set), with no write
> verb of their own. Adding an MCP connection, editing the base prompt,
> widening the allowlist, and model swap stay admin-only and fail-closed.
> Deps 92a, 92g, 116, 61. No `ProtocolVersion` bump (additive methods + wire
> types — Minor-class per `internal/protocol/types/version.go`).
