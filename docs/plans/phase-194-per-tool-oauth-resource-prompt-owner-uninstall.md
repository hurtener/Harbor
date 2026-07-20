# Phase 194 — Per-tool OAuth binding on the resource/prompt paths + owner-scoped credential-sink uninstall

## Summary

Two related closures on the OAuth / credential plane Phase 191 extended. (a)
Phase 191's HA-27b shipped per-tool `oauth_provider` binding scoped to
`callTool` ONLY — `ReadResource` / `GetPrompt` (and the other identity-stamped
MCP RPC paths D-278 lists) still resolve only the connection-level binding, so a
shared MCP server fronting N downstream resources cannot bind a resource-read or
prompt-get to a per-tool audience the way a tool call can. This phase extends
`resolveBearerCtx` / the per-tool `tool_oauth_providers` map to those RPC paths,
re-enforcing every binding rule per entry. (b) GitHub issue #507:
`providerSet.Uninstall(ctx, name)` refuses boot-seeded providers but does NOT
verify the caller's owner matches the installed entry's owner before dropping +
closing it — owner-scoping is enforced entirely caller-side today. This phase
gives `Uninstall` an `Owner` parameter and has `providerSet` refuse a
cross-owner drop (mirroring `Install`'s owner-collision check), so the store
enforces owner-scoping independently — defense-in-depth on the D-303
provider-SET model, closing only the owner's binding and failing that owner's
bound calls loud. Neither closure weakens D-300's credential-sink invariant.

## RFC anchor

- RFC §6.4
- RFC §7

## Briefs informing this phase

- brief 09
- brief 14

## Brief findings incorporated

- brief 14 row 10 (compliance matrix, "Access-token safety"): the per-identity
  southbound bearer and per-tool binding closed the tool-call half; this phase
  closes the same binding on the OTHER identity-stamped RPC paths
  (`ReadResource` / `GetPrompt`) so a resource read or prompt get is
  audience-bound identically to a tool call — the binding is a property of the
  RPC, not just of the `callTool` dispatch site.
- brief 09 "What Harbor must add … Identity-mandatory enforcement": the provider
  "MUST require the components the binding-scope demands … and fail closed on
  missing components." Carried into the owner-scoped uninstall: the store now
  fails closed on a cross-owner drop attempt (`ErrProviderOwnerCollision`-shaped
  refusal) rather than trusting every present-and-future caller to resolve the
  owner first — defense-in-depth, the store enforces the invariant itself.
- brief 09 "Mapping bifrost → Harbor" (custody + owner model): a provider is
  owned by the agent-config revision that installed it; an uninstall is an
  owner-scoped operation. The store-boundary owner check makes that model
  self-enforcing rather than caller-enforced, matching how `Install` already
  refuses an owner collision.
- brief 14 (per-tool binding, one-audience-per-server): 191 closed
  one-audience-per-server for tool CALLS; a shared MCP server also exposes
  resources and prompts against distinct downstream audiences — extending the
  per-tool map to those paths closes the analogous gap for the resource/prompt
  surfaces, re-enforcing the same per-entry binding rules (unknown name / stdio
  transport / static-`Authorization` conflict / downstream-host allow-list).

## Findings I'm departing from (if any)

None. Part (a) implements exactly the resource/prompt extension 191's Non-goals
section reserved ("No resource-read / prompt-get per-tool binding — HA-27b is
scoped to tool calls (`callTool`) only … the other identity-stamped MCP RPC
paths D-278 lists keep resolving the connection-level binding only"). Part (b)
implements exactly issue #507's proposed hardening. Both compose with
D-278/D-300/D-303 and preserve D-300's credential-sink invariant (every new knob
stays boot-declared config-only). No brief finding or settled decision is
contradicted; D-331 is filed as the sanctioned extension, not a re-litigation.

## Goals

- **(a) Resource/prompt per-tool binding.** Extend the per-tool
  `oauth_provider` resolution (`MCPServerConfig.ToolOAuthProviders`, keyed by the
  MCP-side name) to the `ReadResource` and `GetPrompt` RPC paths — and the other
  identity-stamped MCP RPC paths D-278 enumerates — so a resource/prompt request
  resolves its per-entry provider binding (falling back to the connection-level
  `oauth_provider` when unbound), re-enforcing EVERY binding rule per entry
  exactly as HA-27b does for `callTool`.
- **(b) Owner-scoped uninstall.** Give `ProviderSet.Uninstall` an `Owner`
  parameter; `providerSet` refuses the drop when `existing.owner != owner`
  (mirroring `Install`'s `ErrProviderOwnerCollision`), so the store enforces
  owner-scoping independently of the caller — an uninstall closes only the
  owner's binding and fails that owner's bound calls loud. Boot-protected
  (zero-owner) refusal via `ErrProviderBootProtected` stays.
- Preserve D-300's credential-sink invariant across both closures: the per-tool
  map, the resource/prompt binding selection, and the owner check are ALL
  boot-declared / server-derived — none becomes a Protocol-writable
  (agentcfg) credential-sink lever.
- Prove both closures under real drivers + `-race`: a resource read / prompt get
  resolves the correct per-tool bearer with identity propagation; a cross-owner
  uninstall is refused at the store boundary; an owner-scoped uninstall closes
  the right binding and the owner's subsequent bound calls fail loud.

## Non-goals

- No change to who runs the OAuth flow, holds, or refreshes a token — custody
  stays coordinator-side (D-271), unchanged. This phase changes only WHICH
  boot-declared provider a resource/prompt RPC resolves and how the store scopes
  an uninstall.
- No new Protocol-writable credential-sink field. The per-tool map and its
  resource/prompt application stay boot-declared config-only (D-300); a per-tool
  audience selector on the Protocol-writable `ToolExposure` layer was considered
  and rejected in 191 and stays rejected here.
- No auto-widening, auto-escalation, or auto-re-consent on any path — both
  closures are report-not-act on the credential plane; the only write surface
  stays the operator-confirmed discovery-allowance path (D-302), untouched.
- No change to the unified pause/resume primitive (RFC §3.3) — neither closure
  introduces a new pause reason; a broker `consent_required` refusal still parks
  through the existing path.
- No signature verification of an exchanged token, no change to HA-27a's
  audience check or HA-28's actor-token leg (191 shipped those) — this phase
  extends the per-tool binding's REACH (a) and hardens the uninstall boundary
  (b) only.
- No owner parameter change to `Install` (191/D-303 already owner-scopes
  Install); this phase only brings `Uninstall` to parity.

## Acceptance criteria

- [ ] **AC-1** `internal/tools/drivers/mcp`: `resolveBearerCtx(ctx, key)`
      (191's per-tool-aware resolver) is called with a per-key binding on
      EVERY currently-connection-level bearer-injection site — concretely the
      four non-`callTool` `resolveBearerCtx(ctx, "")` call sites in `mcp.go`:
      **`ReadResource`, `SubscribeResource`, the resource-read descriptor
      invoke, and the prompt-get (`GetPrompt`) descriptor invoke**. The
      implementor confirms the set is complete by grepping
      `resolveBearerCtx(ctx, "")` in the driver and covering each site with a
      test (a mechanical completeness gate, not an unlisted "D-278 set"). The
      resource/prompt's addressing key is passed as `key` so
      `cfg.ToolOAuthProviders[key]` resolves, falling back to
      `cfg.OAuthProvider` when unbound. The resource/prompt binding key is
      documented explicitly (the MCP-side resource/prompt name); an unbound
      resource/prompt resolves the connection-level binding, byte-identical to
      today.
- [ ] **AC-2** Every per-entry binding rule HA-27b re-enforces for `callTool` is
      re-enforced identically on the resource/prompt paths: unknown provider
      name, stdio transport (no bearer injection), static-`Authorization`
      conflict, and the D-300 `allowed_downstream_hosts` allow-list — each
      checked per entry at boot validation (not per request), so a misbinding
      fails the boot loud, not silently at first use.
- [ ] **AC-3** `internal/config`: `MCPServerConfig.ToolOAuthProviders` (191's
      map) gains no shape change — the SAME map now applies to resource/prompt
      RPCs. If resource/prompt names can collide with tool names on one server,
      the plan documents the resolution rule (a single per-entry namespace keyed
      by MCP-side name, or a documented precedence) and boot validation rejects
      an ambiguous binding loud. No new Protocol-writable field (D-300).
- [ ] **AC-4** `internal/tools/auth/providers.go`: `ProviderSet.Uninstall` gains
      an `Owner` parameter — `Uninstall(ctx, owner, name)`. `providerSet`
      refuses the drop with `ErrProviderOwnerCollision` (mirroring `Install`'s
      owner-collision check) when `existing.owner != owner`; boot-seeded
      (zero-owner) refusal via `ErrProviderBootProtected` stays; a matching-owner
      drop closes + removes the entry exactly as today.
- [ ] **AC-5** The one caller — `internal/runtime/agentcfg/protocol/removeoauthprovider.go`'s
      `agent_config.remove_oauth_provider` handler — passes the caller's resolved
      owner (the active agent-config revision owner, under `lockAgent`) to
      `UninstallProvider(ctx, owner, name)`. The handler's existing caller-side
      owner resolution stays (defense-in-depth is additive, not a replacement);
      the store check is the second, independent gate.
- [ ] **AC-6** After an owner-scoped uninstall, the owner's subsequent calls
      bound to the dropped provider fail LOUD (the existing bound-call error
      path, unchanged) — never a silent fall-through to an unauthenticated call
      or another owner's provider. A test asserts the loud failure.
- [ ] **AC-7** Cross-owner uninstall refusal is tested at the store boundary
      directly (not only through the handler): owner B's `Uninstall(ctx, ownerB,
      name)` against owner A's installed provider returns
      `ErrProviderOwnerCollision`; owner A's provider is untouched (still
      installed, still resolvable, its bound calls still authenticate).
- [ ] **AC-8** Identity propagation on the resource/prompt binding paths: the
      resolved bearer is scoped to the request's `(tenant, user, session)` +
      invoking `agent_id` exactly as the tool-call path (D-278), proven by a
      concurrent-reuse test extending 191's
      `TestConcurrentReuse_ToolOAuthProviders_NoTokenBleed` to the
      resource/prompt paths — N≥128 distinct triples across BOTH the
      connection-level and per-tool bindings on one `Provider`, asserting no
      cross-tool / cross-identity / cross-RPC token bleed under `-race`.
- [ ] **AC-9** Boot validation (`internal/config`): the resource/prompt
      application of `tool_oauth_providers` adds no new config field but its new
      applicability is validated — a binding declared for a resource/prompt name
      on a stdio transport (no bearer injection) or with a static-`Authorization`
      conflict is rejected loud at boot, same as the tool-call case.
- [ ] **AC-10** `test/integration/` (or in-package mcp driver test with an
      httptest MCP-shaped fixture server): a `ReadResource` / `GetPrompt` against
      a per-tool-bound resource/prompt resolves that provider's bearer (asserted
      on the outbound request), while an unbound resource/prompt on the same
      connection falls back to the connection-level binding; a cross-owner
      uninstall refusal and an owner-scoped uninstall (dropped binding → loud
      bound-call failure) round-trip over the real provider set. Real drivers,
      identity propagation, ≥1 failure mode, `-race`.
- [ ] **AC-11** §17.8 conformance fixture: the resource/prompt binding test
      drives a spec-shaped MCP fixture (an `HARBOR_LIVE_*`-gated stdio MCP
      binary probe where reachable, or a captured transcript committed as the
      fixture) — never a hand-authored fixture encoding the implementer's
      interpretation of the resource/prompt RPC shape.
- [ ] **AC-12** `docs/skills/use-the-harbor-protocol/SKILL.md` and the
      MCP/tools-surfaced skill (grepped per §18) are updated in the same PR: the
      per-tool `oauth_provider` binding now applies to resource reads and prompt
      gets, and `remove_oauth_provider` is owner-scoped at the store boundary.
      `docs/site/protocol/*` regenerated if a wire surface changed
      (`make protocol-docs-gen`); `ProtocolVersion` stays `0.1.0` (no wire
      change is expected — the `remove_oauth_provider` method signature is
      internal; if any canonical view field changes, full D-223/D-209 lockstep).
- [ ] **AC-13** `docs/decisions.md` gains the pre-assigned D-331 entry: the
      resource/prompt reach extension of HA-27b, the owner-scoped `Uninstall`
      hardening (defense-in-depth on D-303, store enforces owner-scoping
      independently), and the explicit statement that neither weakens D-300's
      credential-sink invariant (every knob boot-declared / server-derived).

## Files added or changed

```text
internal/tools/drivers/mcp/mcp.go                # ReadResource/GetPrompt call resolveBearerCtx(ctx, key); other D-278 RPC paths
internal/tools/drivers/mcp/attach.go             # per-tool map validation extended to resource/prompt applicability
internal/tools/drivers/mcp/mcp_test.go           # AC-1/2, per-tool resource/prompt resolution + fallback
internal/tools/drivers/mcp/attach_test.go        # AC-2/9 per-entry rule re-enforcement
internal/tools/auth/providers.go                 # Uninstall(ctx, owner, name) + owner-collision refusal
internal/tools/auth/providers_test.go            # AC-4/6/7 store-boundary owner check
internal/runtime/agentcfg/protocol/removeoauthprovider.go   # pass owner to UninstallProvider (AC-5)
internal/runtime/agentcfg/protocol/removeoauthprovider_test.go
internal/config/validate.go                      # AC-9 resource/prompt binding applicability validation
internal/config/validate_test.go
internal/tools/auth/testdata/                     # §17.8 spec-shaped resource/prompt fixture / transcript + PROVENANCE.md
examples/*.yaml                                   # tool_oauth_providers documented as applying to resource/prompt too
docs/skills/use-the-harbor-protocol/SKILL.md
docs/skills/<mcp/tools skill>/SKILL.md            # grep-identified per §18
docs/site/protocol/*                              # regenerated iff a wire surface changed
test/integration/mcp_oauth_binding_test.go        # AC-10 (or in-package if adequately covered)
docs/decisions.md                                 # D-331
docs/glossary.md                                  # new terms
scripts/smoke/phase-194.sh
```

## Public API surface

```go
// internal/tools/auth/providers.go — Uninstall gains an Owner (owner-scoped)

// Uninstall removes and closes the provider named `name` IFF `owner` matches
// the installed entry's owner. A cross-owner attempt returns
// ErrProviderOwnerCollision (mirroring Install's owner-collision refusal); a
// boot-seeded (zero-owner) entry returns ErrProviderBootProtected. Defense in
// depth: the store enforces owner-scoping independently of caller-side owner
// resolution. Never weakens the D-300 credential-sink invariant.
func (s *providerSet) Uninstall(ctx context.Context, owner Owner, name string) error
// (and the matching ProviderSet interface method)

var ErrProviderOwnerCollision = errors.New("tools/auth: provider owner mismatch")
```

```go
// internal/tools/drivers/mcp — resolveBearerCtx now called on resource/prompt

// resolveBearerCtx(ctx, key) resolves cfg.ToolOAuthProviders[key], falling back
// to cfg.OAuthProvider. 191 wired it for callTool (key = tool name); this phase
// wires it for ReadResource/GetPrompt (key = resource/prompt name) and the
// other identity-stamped RPC paths D-278 lists. Empty key preserves the
// connection-level binding.
func (p *Provider) resolveBearerCtx(ctx context.Context, key string) (context.Context, error)
```

No new config field (191's `ToolOAuthProviders` map is reused). No new Protocol
method. `ProtocolVersion` unchanged (no canonical wire change expected).

## Test plan

- **Unit:**
  - `internal/tools/drivers/mcp`: `resolveBearerCtx` resolves per-resource /
    per-prompt bindings and falls back to the connection binding when unbound;
    each per-entry rule (unknown name / stdio / static-Authorization conflict /
    downstream-host allow-list) re-checked for a resource/prompt binding.
  - `internal/tools/auth/providers.go`: `Uninstall(ctx, owner, name)` — matching
    owner drops + closes; cross-owner returns `ErrProviderOwnerCollision` and
    leaves the entry intact; boot-seeded returns `ErrProviderBootProtected`;
    after an owner-scoped drop the owner's bound-call resolution fails loud.
  - `internal/config/validate.go`: resource/prompt binding applicability
    validation (stdio / static-Authorization / unknown-name rejections at boot).
  - `internal/runtime/agentcfg/protocol/removeoauthprovider.go`: the handler
    passes the resolved owner; a handler-level cross-owner attempt is refused
    (both the caller-side resolution AND the store check are exercised).
- **Integration:** AC-10/AC-11 — an httptest MCP-shaped fixture server (or an
  `HARBOR_LIVE_*`-gated real stdio MCP binary) answering `ReadResource` /
  `GetPrompt`: a per-tool-bound resource resolves that provider's bearer on the
  outbound request; an unbound resource falls back to the connection binding; a
  cross-owner uninstall is refused and an owner-scoped uninstall drops the
  binding → the owner's subsequent bound resource read fails loud. Real
  provider set, identity propagation asserted, `-race`.
- **Conformance:** the §17.8 fixture is spec-derived (captured transcript or
  live-gated probe), never hand-authored.
- **Concurrency / leak:** AC-8 — `TestConcurrentReuse_ToolOAuthProviders_NoTokenBleed`
  extended to the resource/prompt paths, N≥128 distinct triples across
  connection-level + per-tool bindings on one `Provider`, asserting no
  cross-tool / cross-identity / cross-RPC token bleed under `-race`; the
  provider-set uninstall path asserts no goroutine leak on close.

## Smoke script additions

`scripts/smoke/phase-194.sh` (`# PREFLIGHT_REQUIRES: unit-tests`):

- Static greps: `resolveBearerCtx` invoked on the `ReadResource` / `GetPrompt`
  paths in `internal/tools/drivers/mcp/mcp.go` (the per-tool binding actually
  reaches those RPCs, not just described); `Uninstall(` signature carries an
  owner parameter in `internal/tools/auth/providers.go`;
  `ErrProviderOwnerCollision` referenced by the uninstall path.
- `go test ./internal/tools/drivers/mcp/... -run
  'TestProvider.*(ReadResource|GetPrompt).*OAuth|TestConcurrentReuse_ToolOAuthProviders'
  -race` — resource/prompt binding resolution + no-token-bleed.
- `go test ./internal/tools/auth/... -run
  'TestProviderSet_Uninstall.*Owner|TestProviderSet_Uninstall.*Collision' -race`
  — store-boundary owner-scoped uninstall.
- `go test ./internal/runtime/agentcfg/protocol/... -run
  'TestRemoveOAuthProvider.*Owner' -race` — handler passes owner.
- A live-server check where reachable: boot with a `tool_oauth_providers` config
  binding a resource/prompt and assert `mcp.servers.get` reflects the binding
  count unchanged (backward-compatible boot); skips (404/405/501-style)
  gracefully when the fixture isn't present, per the sacred SKIP convention.

## Coverage target

- `internal/tools/drivers/mcp`: 85%
- `internal/tools/auth`: 85%
- `internal/config` (touched): 85%
- `internal/runtime/agentcfg/protocol` (touched): 85%

## Dependencies

- 191

(191 is the sole dependency and is already Shipped: this phase extends 191's
`resolveBearerCtx(ctx, toolName)` per-tool resolver and its
`MCPServerConfig.ToolOAuthProviders` map to the resource/prompt paths, and
hardens the `ProviderSet.Uninstall` boundary 191's provider-set model
(D-303/D-278/D-300) established. The predecessor OAuth/credential phases
28/30/142/148/164/166/168/169 are all long shipped. This phase parallelises in
Stage 1 alongside 192/193.)

## Risks / open questions

- **Resource/prompt vs tool name namespace collision.** One MCP server can
  expose a tool, a resource, and a prompt that share a name. The per-entry map is
  keyed by MCP-side name; the plan must settle whether resource/prompt and tool
  bindings share one namespace or are disambiguated (AC-3). The conservative
  choice — a single per-entry namespace with a documented boot-validation
  rejection of an ambiguous binding — is proposed; confirm against the actual
  MCP RPC addressing shape (§17.8 fixture) during implementation, and never let
  an ambiguous binding resolve silently.
- **Owner type reach.** `Uninstall`'s new `Owner` parameter must match the exact
  `Owner` type `Install` uses; verify no import cycle is introduced by threading
  the owner from the agentcfg handler through the auth provider set (the handler
  already resolves the owner for its caller-side check, so the type is already
  reachable at that boundary).
- **Backward-compat of the `Uninstall` signature.** Adding a parameter is a
  breaking change to the internal `ProviderSet` interface. All callers are
  internal (the single agentcfg handler + tests); no external SDK surface
  exposes `Uninstall`. Confirm the SDK facade (`sdk/`) does not re-export it; if
  it does, the re-export updates in the same PR.
- **No wire change expected, but verify.** The owner-scoped uninstall is an
  internal store change; `remove_oauth_provider`'s request/response shape does
  not gain a field (the owner is server-derived, D-299/D-301). If any canonical
  view field changes, full D-223/D-209 lockstep applies — flagged so the
  implementor checks rather than assumes.

## Glossary additions

- **Per-tool OAuth binding (resource/prompt)** — the extension of HA-27b's
  per-tool `oauth_provider` binding to the `ReadResource` / `GetPrompt` (and
  other identity-stamped) MCP RPC paths, keyed by MCP-side name, re-enforcing
  every per-entry binding rule; boot-declared, never Protocol-writable.
- **Owner-scoped provider uninstall** — the defense-in-depth store-boundary
  check that refuses a cross-owner `Uninstall` (`ErrProviderOwnerCollision`),
  so the credential-sink store enforces owner-scoping independently of
  caller-side owner resolution.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Cross-session isolation test passes (AC-8 no-token-bleed across identities
      on the resource/prompt paths)
- [ ] **If this phase builds a reusable artifact:** N/A — this phase extends the
      existing reusable `mcp.Provider` and `providerSet` (both already
      concurrent-reuse-covered) with additive reach + an owner-scoped uninstall;
      no NEW compiled artifact is introduced. AC-8 extends 191's existing
      no-token-bleed test rather than opening a new D-025 surface.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a
      cross-subsystem seam:** yes — the integration test (AC-10) wires the real
      MCP driver + real provider set + real agentcfg handler end-to-end, asserts
      identity propagation, covers ≥1 failure mode (cross-owner uninstall
      refusal + loud bound-call failure after an owner-scoped drop), under
      `-race`; §17.8 fixture is spec-derived (AC-11).
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry
      filed (N/A — no departure; D-331 filed as the sanctioned extension)
