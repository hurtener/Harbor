# Phase 191 — OAuth broker legs: step-up visibility, resource-bound exchange, per-tool binding, actor chain (+ wave E2E)

## Summary

Three additive legs on the existing broker-pull spine (discovery / D-297,
live confirm-writes / D-302, per-identity southbound bearer / D-278,
tokenexchange PULL / D-271): (HA-26) a downstream 403 +
`WWW-Authenticate: insufficient_scope` step-up becomes structured data
instead of an opaque tool error; (HA-27) the RFC 8707 resource indicator
rides the RFC 8693 exchange with audience verification, and the
`oauth_provider` binding gains a tool-granularity axis on one MCP
connection; (HA-28) the run's verified acting principal optionally rides
the exchange as an RFC 8693 `actor_token`. All three are report-not-act:
custody, acquisition, refresh, and consent stay coordinator-side; nothing
here widens a binding or runs a flow. This phase also bundles the v1.16
wave-end E2E across phases 185–190.

## RFC anchor

- RFC §6.4
- RFC §5.2
- RFC §7

## Briefs informing this phase

- brief 09
- brief 14

## Brief findings incorporated

- brief 09 "Mapping bifrost → Harbor": "Acting subject = `agent_id`;
  requesting principal = `user_id`. Provenance captures both." This is the
  literal shape HA-28's `actor_token` leg implements: the tokenexchange
  driver's existing `subject_token` already encodes the requesting
  principal (the verified `(tenant, user, session)` triple via
  `encodeSubjectToken`, `tokenexchange.go:943`); the new `actor_token` adds
  the acting principal (`agent_id`, when the run carries one) as a
  distinct RFC 8693 field so the broker's AS can bind/cross-check the two
  instead of only ever seeing the subject.
- brief 09 "What Harbor must add… Identity-mandatory enforcement": the
  provider "MUST require the components the binding-scope demands… and
  fail closed on missing components." Carried forward directly: an
  absent `agent_id` on ctx means no `actor_token` is sent (never a
  fabricated or client-supplied one) — HA-28's "never a client-named
  field" constraint traces straight back to this brief's identity-mandatory
  rule plus D-219 (authority from verified ctx, not the request body).
- brief 14 row 10 (compliance matrix, "Access-token safety"): *"Partial —
  Static bearer via header; never query-string — but no resource binding /
  audience check."* This is HA-27's exact, named gap. The brief's row was
  written against the pre-85b driver; the resource-binding half is still
  open even after 148/164/166/168/169 landed the bearer-injection,
  discovery, and credential-sink halves — HA-27 closes it.
- brief 14 row 9: *"OAuth for HTTP servers — Absent (for MCP)… No RFC 9728,
  no RFC 8707."* RFC 8707 (resource indicators) was named as a gap at
  brief-authoring time; Phase 164/D-297 later shipped the DISCOVERY half
  (an advertised `resource` value surfaces on
  `MCPAuthorizationServerView.Resource`, inert). HA-27 closes the other
  half brief 14 flagged: carrying a resource indicator ON the exchange
  request, not just reporting one from discovery.

## Findings I'm departing from (if any)

None. This phase's design point — surfacing a scope shortfall as inert,
report-only data rather than acting on it — is the SAME posture brief 09
and D-297 already established for `tool.auth_required` / discovered OAuth
requirements; no brief finding is contradicted.

## Goals

- HA-26: a downstream 403 + `WWW-Authenticate: insufficient_scope` (or a
  tokenexchange scope-shortfall) is structured data on the tool-result
  error path and the MCP connection view — never an opaque string.
- HA-27a: a tokenexchange-bound provider can carry a boot-declared RFC 8707
  `resource` value on the exchange request, with best-effort audience
  verification on the returned token (confused-deputy defense, honestly
  scoped to what an opaque bearer permits).
- HA-27b: one MCP connection's `oauth_provider` binding can be overridden
  per-tool (server-side tool name), falling back to the connection-level
  binding — closing the one-audience-per-server constraint for a shared
  MCP server fronting N downstream resources.
- HA-28: a tokenexchange-bound provider can optionally carry the run's
  verified acting principal (`agent_id`, when present) as an RFC 8693
  `actor_token`, backward-compatible when absent.
- Bundle the v1.16 wave-end E2E (`test/integration/wave_v116_test.go`) per
  CLAUDE.md §17.7 step 5.

## Non-goals

- No change to who runs the OAuth flow, holds, or refreshes a token —
  custody stays coordinator-side (D-271) for every leg in this phase.
- No auto-escalation, auto-re-consent, or auto-widened binding on an
  observed scope shortfall (HA-26 is report-not-act; the operator-driven
  discovery-allowance-write path, D-302, is the only write surface and is
  untouched here).
- No weakening of D-300's credential-plane invariant: `resource`,
  `actor_token` participation, and the per-tool `oauth_provider` map are
  ALL boot-declared, config/file-only — none becomes a Protocol-writable
  (agentcfg) field. A per-tool audience selector living on the
  Protocol-writable `agentcfg.ToolExposure` layer was considered and
  rejected: it would reopen exactly the admin-writable-credential-sink
  hole D-300 closed, one layer down (an admin picks which of N
  boot-declared providers a tool's calls authenticate under — still a
  sink-adjacent choice — but the SET of eligible providers, their
  audiences, their scope ceilings, and their downstream-host allow-lists
  stay boot-declared; see Risks).
- No touch to the unified pause/resume primitive (RFC §3.3). None of the
  three legs introduces a new pause reason: HA-26 is inert reporting: HA-27
  and HA-28 change only the exchange REQUEST shape and post-exchange
  verification; a broker `consent_required` refusal still parks through
  the existing `buildConsentRequired` path, untouched.
- No signature verification of the exchanged token's JWT (if JWT-shaped)
  — Harbor has no keying relationship with the broker's AS to verify a
  signature; the audience check in HA-27a is a `aud`-claim comparison only,
  a confused-deputy defense, never a trust-establishing verification.
- No resource-read / prompt-get per-tool binding — HA-27b is scoped to
  tool calls (`callTool`) only, matching the ask's literal "per-tool" axis;
  the other four identity-stamped MCP RPC paths (`ReadResource`,
  `GetPrompt`, the two additional call sites D-278 lists) keep resolving
  the connection-level binding only.
- No new ErrorClass value beyond what fixes the retry-storm bug named
  below; no new Protocol method.

## Acceptance criteria

- [ ] A 403 (or a JSON-RPC `insufficient_scope`-shaped tool error) from an
      MCP call answers with a typed `tools.ErrInsufficientScope` carrying
      the downstream resource id, the challenge's required scope(s), the
      binding's most-recently-granted scope(s), the verbatim
      `WWW-Authenticate` value, and the origin — surfaced on the SAME
      tool-result error path every transport already uses
      (`internal/tools/lifecycle.go`'s `publishToolOutcome` /
      `tool.failed`'s additive `ScopeShortfall` field), never a bare string.
- [ ] The MCP connection view (`mcp.servers.get`) additionally surfaces the
      last observed shortfall as `MCPServerView.LastScopeShortfall`,
      mirroring how `OAuthRequirement` already surfaces discovery — even
      when the originating call itself was never observed by the current
      reader.
- [ ] `tools.ClassifyError` recognizes `*tools.ErrInsufficientScope` and
      returns `ErrClassPermanent` — closing a latent bug where an
      unclassified 4xx-shaped error falls through to the conservative
      `ErrClassTransient` default and gets retried against a scope
      shortfall that retrying can never fix.
- [ ] A `tokenexchange`-bound provider whose config declares
      `resource_indicator` carries it as the RFC 8707 `resource` form
      parameter on every exchange; a JWT-shaped returned access token
      whose `aud` claim excludes the declared resource fails the exchange
      loud (`ErrAudienceMismatch`, never cached); an opaque returned token
      records `AudienceVerified: false` on `tool.credential_exchanged`
      rather than a false-positive pass.
- [ ] `MCPServerConfig.tool_oauth_providers` (server-side tool name →
      declared provider name) resolves and validates exactly like the
      connection-level `oauth_provider` binding (unknown name / stdio
      transport / static-`Authorization` conflict / downstream-host
      allow-list all re-enforced per entry); a `callTool` for a bound tool
      name resolves that provider's bearer; an unbound tool name on the
      same connection falls back to the connection-level binding
      unchanged.
- [ ] A `tokenexchange`-bound provider whose config declares
      `include_actor_token: true` and whose ctx carries an invoking
      `agent_id` (`tools.InvokingAgentFrom`) sets `actor_token` +
      `actor_token_type` on the exchange; absent `agent_id`, or
      `include_actor_token` unset, produces byte-identical request shape
      to today.
- [ ] Every new config field is additive, optional, and documented in
      `examples/`; boot validation rejects `resource_indicator` /
      `include_actor_token` / `tool_oauth_providers` set on a non-
      `tokenexchange` / non-MCP context where they have no meaning.
- [ ] `test/integration/wave_v116_test.go` proves, over real drivers,
      under `-race`, N≥10 concurrency: a `Batch` spawn+tools turn (185/186),
      a meta-tool cancel under the operator/agent/cascade hierarchy (187),
      a background-wake notification reaching the conversation surface
      (188), cache read/write tokens captured on a real completion (189),
      the `agents.list` default-agent row (190), and one OAuth-leg failure
      mode from this phase (an insufficient-scope call surfacing structured
      data) — with identity propagation asserted throughout.
- [ ] Full D-223/D-209 lockstep for the one genuinely new canonical wire
      field (`MCPServerView.LastScopeShortfall` +
      `MCPScopeShortfallView`); `ProtocolVersion` stays `0.1.0`.
- [ ] `use-the-harbor-protocol` and any MCP/tools-surfaced skill
      (grepped per §18) are updated in the same PR.

## Files added or changed

```text
internal/tools/tools.go                                   # ErrInsufficientScope + ScopeShortfallDetail
internal/tools/lifecycle.go                                # publishToolOutcome: new errors.As case
internal/tools/policy.go                                   # ClassifyError: new case → ErrClassPermanent
internal/tools/events.go                                   # ToolFailedPayload.ScopeShortfall (additive)
internal/tools/drivers/mcp/oauth_challenge.go               # capture 403 + error/scope params; per-call ctx slot
internal/tools/drivers/mcp/mcp.go                           # callTool: read capture slot; resolveBearerCtx(ctx, toolName)
internal/tools/drivers/mcp/attach.go                        # resolveOAuthBinding: per-tool map validation
internal/tools/drivers/mcp/registry.go                      # RecordScopeShortfall (mirrors RecordAuthChallenge)
internal/tools/drivers/mcp/config.go (or mcp.go)             # Config.ToolOAuthProviders map[string]auth.OAuthProvider
internal/protocol/types/mcp_servers.go                      # MCPScopeShortfallView + MCPServerView.LastScopeShortfall
internal/protocol/mcp.go                                    # projector: LastScopeShortfall onto the detail read
internal/config/config.go                                   # MCPServerConfig.ToolOAuthProviders; ToolOAuthProviderConfig.{ResourceIndicator,IncludeActorToken}
internal/config/validate.go                                 # validation for the three new fields
internal/tools/auth/drivers/tokenexchange/tokenexchange.go  # resource param, audience check, actor_token
internal/tools/auth/drivers/tokenexchange/*_test.go         # new coverage
internal/tools/auth/events.go                                # ToolCredentialExchangedPayload.{AudienceVerified,ActorAsserted}
internal/tools/auth/testdata/oauthdiscovery/www_authenticate_insufficient_scope.txt  # RFC 6750 §3.1 spec-shaped fixture
internal/tools/auth/testdata/oauthdiscovery/PROVENANCE.md    # new fixture entry
examples/*.yaml                                              # new config fields documented
docs/skills/use-the-harbor-protocol/SKILL.md                 # LastScopeShortfall + tool-result envelope note
docs/skills/<mcp/tools skill>/SKILL.md                        # per-tool oauth_provider + resource_indicator note (grep-identified)
docs/site/protocol/types.md                                   # regenerated (make protocol-docs-gen)
docs/glossary.md                                               # 4 new terms
test/integration/wave_v116_test.go                            # wave-end E2E
scripts/smoke/phase-191.sh
```

## Public API surface

```go
// internal/tools/tools.go

// ErrInsufficientScope is the typed, structured signal for a downstream
// step-up scope shortfall (RFC 6750 §3.1: 403 + WWW-Authenticate carrying
// error="insufficient_scope"). Report-not-act: constructing this value
// never escalates, re-consents, or widens a binding — it is data for the
// coordinator to act on via the existing discovery-allowance-write path.
type ErrInsufficientScope struct {
    Source             ToolSourceID
    ToolName           string
    DownstreamResource string   // origin/host the challenge came from
    RequiredScopes     []string // parsed WWW-Authenticate `scope` param
    GrantedScopes      []string // the binding's most-recently-granted scopes
    WWWAuthenticate    string   // verbatim header value
    Origin             string   // scheme://host[:port]
}
func (e *ErrInsufficientScope) Error() string

// ScopeShortfallDetail is the wire-safe (SafePayload) projection of
// ErrInsufficientScope carried on ToolFailedPayload.
type ScopeShortfallDetail struct {
    DownstreamResource string
    RequiredScopes     []string
    GrantedScopes      []string
    WWWAuthenticate    string
    Origin             string
}

// internal/tools/events.go — additive field
type ToolFailedPayload struct {
    events.SafeSealed
    Identity       identity.Quadruple
    ToolName       string
    Transport      TransportKind
    Attempts       int
    ErrorClass     ErrorClass
    ErrorMessage   string
    ScopeShortfall *ScopeShortfallDetail `json:",omitempty"` // NEW
}
```

```go
// internal/protocol/types/mcp_servers.go

// MCPScopeShortfallView is the last observed insufficient_scope step-up
// on this connection, surfaced as inert data on the detail read.
type MCPScopeShortfallView struct {
    ToolName           string    `json:"tool_name"`
    DownstreamResource string    `json:"downstream_resource"`
    RequiredScopes     []string  `json:"required_scopes"`
    GrantedScopes      []string  `json:"granted_scopes"`
    WWWAuthenticate    string    `json:"www_authenticate"`
    Origin             string    `json:"origin"`
    ObservedAt         time.Time `json:"observed_at"`
}

// MCPServerView gains, additive:
//   LastScopeShortfall *MCPScopeShortfallView `json:"last_scope_shortfall,omitempty"`
```

```go
// internal/config/config.go — additive fields, restart-required

type ToolOAuthProviderConfig struct {
    // ... existing fields unchanged ...

    // ResourceIndicator is the boot-declared RFC 8707 `resource` value
    // carried on every tokenexchange exchange request. Empty preserves
    // today's behaviour (no resource param sent). Ignored by `oauth2`.
    // NEVER auto-populated from discovery (D-297 report-don't-follow) —
    // an operator copies a discovered+confirmed value in by hand.
    ResourceIndicator string `yaml:"resource_indicator,omitempty"`

    // IncludeActorToken opts a tokenexchange provider into carrying the
    // run's verified acting principal (agent_id, when present) as an
    // RFC 8693 actor_token. Default false; absent agent_id on ctx sends
    // no actor_token regardless. Ignored by `oauth2`.
    IncludeActorToken bool `yaml:"include_actor_token,omitempty"`
}

type MCPServerConfig struct {
    // ... existing fields unchanged ...

    // ToolOAuthProviders are per-tool oauth_provider overrides keyed by
    // the MCP tool's server-side name (mirrors ToolPolicies' shape). A
    // tool named here binds THAT provider for its CallTool RPCs only;
    // an unlisted tool falls back to OAuthProvider (the connection-level
    // binding). Each entry re-enforces the same binding rules as
    // OAuthProvider (unknown name / stdio transport / static-Authorization
    // conflict / downstream-host allow-list). Restart-required.
    ToolOAuthProviders map[string]string `yaml:"tool_oauth_providers,omitempty"`
}
```

```go
// internal/tools/drivers/mcp/mcp.go — signature change (internal, no
// external Public API break: resolveBearerCtx is unexported)

func (p *Provider) resolveBearerCtx(ctx context.Context, toolName string) (context.Context, error)
// toolName == "" (resource/prompt paths) always resolves cfg.OAuthProvider.
// toolName != "" (callTool) resolves cfg.ToolOAuthProviders[toolName],
// falling back to cfg.OAuthProvider when absent.
```

## Test plan

- **Unit:**
  - `internal/tools/oauth_challenge_test.go` / `oauth_challenge_test.go`
    (mcp driver): 403 capture fires alongside 401; `error`/`scope`
    challenge params parse per RFC 6750 §3.1; a non-Bearer / non-401/403
    response is a no-op (unchanged).
  - `internal/tools/lifecycle_test.go`: `publishToolOutcome` emits
    `ScopeShortfall` on `*tools.ErrInsufficientScope`, nil otherwise.
  - `internal/tools/policy_test.go`: `ClassifyError(*ErrInsufficientScope)`
    → `ErrClassPermanent`; retry shell makes exactly one attempt.
  - `tokenexchange_test.go`: `resource` form param present iff
    `ResourceIndicator` set; JWT-shaped mismatched-`aud` token fails the
    exchange with `ErrAudienceMismatch`, nothing cached; opaque token
    records `AudienceVerified:false`, exchange still succeeds;
    `actor_token`/`actor_token_type` present iff `IncludeActorToken` AND
    an invoking `agent_id` is on ctx; absent either → byte-identical
    request to the pre-phase shape (a golden-form-body test).
  - `attach_test.go`: `tool_oauth_providers` entry resolution + every
    existing binding-rule rejection re-checked per entry; an unbound tool
    name falls back to the connection provider.
  - `config/validate_test.go`: new-field validation (driver mismatch,
    empty-map handling, unknown tool name warning-vs-fail decision made
    explicit).
- **Integration:**
  - `internal/tools/drivers/mcp/*_test.go`: an httptest MCP-shaped fixture
    server answering 403 + a spec-shaped `WWW-Authenticate` on `CallTool`
    → the driver's returned error `errors.As`-unwraps to
    `*tools.ErrInsufficientScope` with every field populated; the
    connection registry's `mcp.servers.get` projection carries
    `LastScopeShortfall` afterward.
  - `test/integration/wave_v116_test.go` (real drivers, `-race`, N≥10
    concurrency): the six-surface proof listed in Acceptance Criteria,
    identity propagation asserted on every leg, plus the OAuth-leg failure
    mode (an insufficient-scope call against the fixture server, tenant-
    isolated across ≥2 tenants running the same scenario concurrently with
    no cross-talk).
- **Conformance:** N/A — no new persistence-shaped subsystem.
- **Concurrency / leak:** `TestConcurrentReuse_ToolOAuthProviders_NoTokenBleed`
  extends the existing `TestConcurrentReuse_OAuthBearer_NoTokenBleed`
  (D-278) pattern to N≥128 distinct triples across BOTH the connection-
  level and per-tool bindings on one `Provider` instance, asserting no
  cross-tool / cross-identity token bleed under `-race`.

## Smoke script additions

- `scripts/smoke/phase-191.sh`: unit-test class assertions (the phase adds
  no new Protocol method/REST endpoint, so no live-server curl step is
  load-bearing per se) plus a live-server check where reachable: boot with
  a `tokenexchange` provider declaring `resource_indicator` +
  `tool_oauth_providers` against the preflight dev server's fixture config,
  assert `mcp.servers.get` returns `oauth_binding_count` unchanged
  (backward-compatible boot) and, when the fixture MCP server is wired to
  answer 403 on one tool, assert `LastScopeShortfall` appears on the
  detail read after one invocation. Skips (404/405/501-style) gracefully
  when the fixture server isn't present in a given build, per the sacred
  SKIP convention.
- Runs `TestE2E_WaveV116` with a no-match-fails guard under `-race`.

## Coverage target

- `internal/tools`: 85%
- `internal/tools/drivers/mcp`: 85%
- `internal/tools/auth/drivers/tokenexchange`: 85%
- `internal/protocol/types` (touched): 85%
- `internal/config` (touched): 85%

## Dependencies

- 28 (MCP southbound base)
- 30 (tool-side OAuth + HITL via pause/resume — `auth.OAuthProvider`,
  `ErrAuthRequired`, the pause primitive this phase does NOT touch)
- 142 (D-271 — the `tokenexchange` driver this phase extends)
- 148 (D-278 — per-identity southbound bearer / HA-1: `resolveBearerCtx`,
  the connection-level `oauth_provider` binding this phase adds a
  per-tool axis onto)
- 164 (D-297 — MCP OAuth requirement discovery / HA-11: the RFC 9728/8414
  chain and the `resource` field on `MCPAuthorizationServerView` an
  operator copies into `resource_indicator`)
- 166 (D-300 — credential-sink hardening: the `allowed_downstream_hosts`
  allow-list and the boot-declared-sink invariant this phase composes
  with, never weakens)
- 168 (D-302 — live MCP OAuth discovery-allowance write / HA-15: the
  operator-confirmed-write pattern this phase's config additions follow)
- 169 (D-303 — Protocol-installed OAuth provider: the zero-URL broker-pull
  descriptor shape whose invariant — no admin-writable field determines a
  credential sink — this phase's non-goals explicitly preserve)
- 185, 186, 187, 188, 189, 190 (wave-end E2E only — the Batch decision +
  executor, task-management meta-tools, background-wake/failure honesty,
  cache-token capture, and the default-agent row this phase's
  `wave_v116_test.go` exercises alongside its own OAuth-leg failure mode)

## Risks / open questions

- **Per-tool `oauth_provider` at the tools.entries[]/agentcfg layer was
  considered and rejected.** The task framing suggested `ToolExposure`
  (the Protocol-writable, admin-revisioned per-tool layer, Phase 151/
  D-281) as a plausible home. It is NOT used here: `ToolExposure` is
  reachable via `agent_config.set_tool_exposure`, a Protocol write: a
  per-tool audience/provider SELECTION is a credential-sink-adjacent
  choice, and D-300's invariant is that no admin-writable field
  determines a credential sink. The existing `tools.entries[].oauth`
  mechanism was also considered and rejected for MCP tool-call injection
  specifically: it is a pre-check-only wrapper
  (`internal/tools/catalog/catalog.go:475` `WrapWithOAuth` — "The wrapper
  does NOT inject the token into the upstream request") wired at the
  catalog-descriptor level, decoupled from the MCP driver's own
  connection-scoped `bearerInjectingTransport`; using it for injection
  would silently pre-check one provider's token while the transport
  injects a DIFFERENT (connection-bound) one. `MCPServerConfig.
  tool_oauth_providers` — boot-declared, config/file-only, mirroring
  the existing `ToolPolicies` per-tool-override shape exactly — is the
  only mechanism that actually reaches injection at the RPC-dispatch
  site (`callTool`, which already has the tool name in scope) without
  reopening a Protocol-writable credential-sink lever.
- **Opaque-token audience verification is a documented ceiling, not a
  gap to silently paper over.** RFC 8693 does not require the exchanged
  access token to be JWT-shaped; most broker implementations issue
  opaque bearer strings. HA-27a's audience check is real when the token
  IS JWT-shaped and a documented no-op (`AudienceVerified:false`, never
  a fabricated pass) otherwise — the class-rule precedent (absence made
  representable, D-311-adjacent) rather than a false confidence signal.
- **The per-call scope-shortfall ctx capture slot is new plumbing, not a
  reuse of the existing best-effort `OnAuthChallenge` callback.** The
  existing 401 capture (`challengeCapturingTransport`) is deliberately
  decoupled from the call's own error return ("NEVER alters the call's
  error semantics"). HA-26 needs the SAME call's error enriched, so this
  phase adds a request-scoped ctx slot the transport writes and `callTool`
  reads after the call returns — per-run state on ctx, never provider-
  level mutable state (D-025). The async registry-level capture (extended
  to 403) is kept in parallel so the connection view still gets populated
  even when a specific call wasn't observed live by the current reader.
- **A single 403 response is ambiguous** between "insufficient_scope" and
  an ordinary authorization-policy 403 unrelated to OAuth scope. The
  driver only constructs `ErrInsufficientScope` when the challenge's
  `error` param is literally `insufficient_scope` (RFC 6750 §3.1); an
  unmarked 403 stays today's opaque `ErrTransportFailed`, avoiding a
  false-positive structured signal on a server that just returns bare
  403s.

## Glossary additions

- **insufficient-scope step-up**
- **RFC 8707 resource indicator (tokenexchange)**
- **per-tool OAuth binding (MCP)**
- **RFC 8693 actor token**

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Cross-session isolation test passes (the wave E2E's N≥10 concurrency
      stress covers this phase's legs alongside 185–190's surfaces)
- [ ] N/A — this phase extends existing reusable artifacts
      (`tokenexchange.provider`, `mcp.Provider`) with additive fields only;
      no NEW compiled artifact is introduced. The existing D-025
      concurrent-reuse tests for both are extended (see Test plan
      "Concurrency / leak"), not newly created.
- [ ] Integration test exists (`test/integration/wave_v116_test.go` +
      in-package MCP driver tests), wires real drivers end-to-end, asserts
      identity propagation, covers ≥1 failure mode, runs under `-race`.
- [ ] Glossary updated (4 terms above)
- [ ] N/A — no brief finding departed from (see "Findings I'm departing
      from" above)
