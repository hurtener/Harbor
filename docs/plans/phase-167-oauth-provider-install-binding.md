# Phase 167 — Protocol-installed OAuth provider descriptor (non-secret broker-pull shape) + the connection→provider binding

> Part of the v1.14 wave (`docs/plans/wave-v114-coordination.md`). Decision:
> **D-301**. The second half of HA-15 — Phase 166 ships the discovery-allowance
> write; this phase ships the provider-descriptor install and the
> connection→provider binding affordance.

## Summary

A runtime-added MCP connection can already NAME an OAuth provider — the
non-secret `oauth_provider` binding rides both `agentcfg.MCPConnectionDescriptor`
(`internal/agentcfg/agentcfg.go:234`) and `AttachRequest`
(`internal/runtime/agentcfg/protocol/addconnection.go:71-93`) — but the provider
it names must already exist, and the provider LIST is built ONCE at boot from
`tools.oauth_providers[]` (`auth.BuildProviders` → a plain
`map[string]auth.OAuthProvider` handed by value to the attacher and the catalog
builder). So a Protocol-driven consumer can bind a connection to a provider it
has no way to install. This phase closes that: a NON-SECRET, broker-pull-only
provider descriptor becomes installable over the Protocol on the agent-config
revision spine, backed by a §4.4 provider-registry seam that the runtime attach
path consults live. The Protocol-writable shape deliberately **cannot carry an
environment-variable name** — `client_id_env`, `client_secret_env`, and
`remote.auth_token_env` are REJECTED, because an admin who can write an env-var
name plus a `token_url` owns an env-var exfiltration primitive. The process
secret stays config/file-only, reached by NAME through a boot-declared credential
broker. The binding half is genuinely small: the wire already carries it; the
Console silently DROPS it (`state.svelte.ts:912-923`) — this phase stops
dropping it and pins the round-trip with a test.

## RFC anchor

- RFC §6.4 — the tool catalog, its transports, and the tool-side OAuth provider
  seam (`internal/tools/auth`) this phase makes runtime-registrable.
- RFC §6.16 — the agent-config control plane (the revision spine the install
  rides: versioned, diffable, rollback-able, under `lockAgent`).
- RFC §5.2 — what the Protocol exposes (the two additive admin verbs + the
  additive config section).
- RFC §6.15 — the governance / audit posture (admin-scope audit on install and
  uninstall; fail-closed on an un-auditable write).
- RFC §7 — the Console lens (the provider card + the Add-connection binding
  select).

## Briefs informing this phase

- brief 09
- brief 14
- brief 11

## Brief findings incorporated

- **brief 09 §"What to lift from bifrost (concrete)" item 2 (RFC 7591 dynamic
  registration):** "Implementing it once means operators don't have to
  hand-register a client app per server." The same argument applies one level
  up: a consumer that brokers credentials centrally should not have to
  hand-edit yaml + restart to register the BROKER. This phase makes the
  provider descriptor installable — but only in the shape that carries no
  process secret (see the departure note below).
- **brief 09 §"the dynamic-registration footguns":** registration failures fail
  loud — no silent fallback to an unauthenticated dial. Applied to the install
  path: an unknown credential-broker name, a duplicate of a boot-declared
  provider name, and a secret-bearing field each fail loud with a distinct typed
  error; and — the load-bearing case — uninstalling a provider CLOSES it, so a
  still-bound connection's next call fails LOUD rather than degrading to an
  unauthenticated dial (§13 "silent degradation" is forbidden).
- **brief 09 §PKCE / §"What bifrost provides":** the interactive OAuth flow
  needs a redirect, a browser, and a per-user consent — none of which a
  Protocol-installed descriptor can safely bootstrap without a process secret.
  This is one of the reasons the Protocol-writable shape is restricted to the
  NON-interactive `tokenexchange` PULL driver (D-271).
- **brief 14 §item 9 ("OAuth for HTTP servers"):** Harbor's MCP HTTP auth
  posture is config-declared. This phase does not add a second auth mechanism —
  it makes the EXISTING provider seam (`auth.OAuthProvider`, D-083; the
  `tokenexchange` PULL driver, D-271; the credential-source seam, D-285)
  reachable from the control plane. No parallel provider construction path.
- **brief 11 §"MCP Connections view" / §"OAuth & Auth tab":** the Console's
  OAuth affordances are pure consumers of the shipped provider flow — never a
  parallel binding-state machine. Held here: the Console gains a provider card
  and a binding SELECT populated from the provider list; it does not model
  provider state itself.

## Findings I'm departing from (if any)

- **brief 09 is positive on RFC 7591 dynamic registration and on
  operator-config reduction generally; this phase deliberately does NOT make
  the interactive `oauth2` driver (or any env-named local-secret provider)
  Protocol-installable.** The departure is a security judgement the brief does
  not consider: `ToolOAuthProviderConfig.ClientIDEnv` / `ClientSecretEnv`
  (`internal/config/config.go:1086-1109`) and `ToolOAuthRemoteConfig.AuthTokenEnv`
  (`:1134-1154`) name ENVIRONMENT VARIABLES OF THE RUNTIME PROCESS, and the same
  descriptor carries a `token_url` / `remote.url`. An `admin` caller able to
  write both fields can point the URL at a host they control, name any env var
  in the runtime's environment, and receive its value in the outbound request —
  an env-var exfiltration primitive, from a scope that is otherwise
  configuration-shaped. So the Protocol-writable descriptor is restricted to the
  shape that names NO env var at all, and the process secret is reached by NAME
  through a boot-declared, config/file-only credential broker. Env-named
  local-secret providers stay config-only (yaml + restart). Recorded in D-301;
  this is a departure from the brief's "reduce operator config burden" framing,
  taken knowingly and bounded.

## Goals

- **A NON-SECRET, Protocol-writable provider descriptor.** The wire shape is
  exactly:

  ```text
  {name, driver: "tokenexchange", credential_source: "remote",
   credential_broker: "<boot-declared broker name>",
   token_url, auth_url?, scopes[]}
  ```

  Every field is non-secret: a name, a driver id, a broker NAME (a selector, not
  a value — the same name-indirection `MCPServerConfig.OAuthProvider` and
  `ToolEntryConfig.OAuth.Provider` already use), and endpoint URLs. **No
  `client_id_env`, no `client_secret_env`, no `remote.auth_token_env`, no
  `remote.url`, no literal secret of any kind.**

- **The boot-declared credential broker (the ONE place the process secret is
  named).** Config gains `tools.oauth_credential_brokers[]` — a NAMED list whose
  entry is the EXISTING `ToolOAuthRemoteConfig` shape plus a `name`
  (`{name, url, auth_token_env, cache_ttl?, timeout?}`). Boot-declared,
  restart-required, config/file-only, never Protocol-writable. An installed
  provider references one by name; an unknown name fails loud naming the
  declared set (the §4.4 factory error posture). The existing INLINE
  `oauth_providers[].remote` block stays valid and unchanged — this is additive,
  backward-compatible, and does NOT fork the credential-source seam (D-285): a
  broker-bound provider resolves through the SAME `remote` credential source,
  it just gets its `ToolOAuthRemoteConfig` by name instead of inline.

- **Reject secret-bearing fields LOUDLY (hard constraint 1).** A write carrying
  `client_id_env`, `client_secret_env`, or `remote.auth_token_env` — at ANY
  nesting — is rejected with a typed `CodeInvalidRequest` naming the offending
  field and explaining why. NEVER silently ignored, never stripped-and-accepted
  (a silent strip would let a caller believe an env-named provider was installed
  and then fail confusingly at first token need). Named test:
  `TestSetOAuthProvider_RejectsEnvNamedSecretFields` — a table over all three
  fields, asserting the typed error AND that nothing was written to the
  revision.

- **The §4.4 provider-registry seam (this is the real work).** Today
  `auth.BuildProviders` returns a plain `map[string]auth.OAuthProvider` that is
  handed BY VALUE into `mcpdrv.AttachDeps.OAuthProviders`
  (`internal/tools/drivers/mcp/attach.go:81`, consumed by `resolveOAuthBinding`
  at `:119`) and into `catalog.Deps.OAuthProviders`
  (`internal/tools/catalog/catalog.go:168-173`). A runtime-installed provider is
  invisible to both. This phase introduces `auth.ProviderRegistry` — an
  interface (`Get(name) (OAuthProvider, bool)`, `Names() []string`,
  `Install(name, OAuthProvider) error`, `Uninstall(ctx, name) error`) with ONE
  concrete, internally-synchronised (D-025), seeded at boot from
  `BuildProviders`. **Scoped blast radius:** the MCP ATTACH path
  (`AttachDeps` + `MCPConnectionAttacher`) consults the registry — that is the
  path a runtime-added connection takes. The CATALOG BUILDER keeps its boot map:
  its `tools.entries[]` bindings are boot-declared and restart-required by
  design, and widening them is not in this phase's ask. Named explicitly so the
  boundary is a decision, not an omission. This realises, for the provider LIST
  only, the seam the parked Phase 92k reserved (see the sibling reconciliation
  below).

- **INSTALL and UNINSTALL ship together (hard constraint 3 — the D-287
  lesson).** v1.11 shipped a detach-only reconcile and ate the asymmetry; this
  phase does not repeat it. Two verbs:
  - `agent_config.set_oauth_provider` — upsert by name (a new revision;
    siblings carried forward under the D-283 guard) + live `Install`.
  - `agent_config.remove_oauth_provider` — drop by name (a new revision) + live
    `Uninstall`, which CLOSES the provider (`OAuthProvider.Close` — invalidating
    its in-memory credential cache).

  **Uninstall has real, LOUD live effect.** A connection attached with a binding
  holds a reference to the provider object (`resolveOAuthBinding` resolves at
  ATTACH time), so uninstalling does not magically un-bind the live transport.
  What it MUST do — and what is test-pinned — is make the bound connection's
  next identity-stamped call FAIL LOUD with a typed error rather than fall back
  to an unauthenticated dial (§13). `Close` on the provider is what produces
  that; a closed provider's `Token` returns a typed error. The Console copy and
  the method godoc both say this in plain words: *removing a provider that a
  live connection is bound to breaks that connection's calls until it is removed
  or re-added.* No silent half-write, no silently-degraded auth.

- **Rollback past an install is the SAME mechanism.** Rolling the agent config
  back to a revision that predates a provider install runs the same
  `Uninstall` through the run-start reconcile seam
  (`internal/runtime/agentcfg/projection/projection.go` — beside
  `ReconcileConnections` at `:141`). One mechanism, N triggers (§13) — never a
  second teardown path.

- **The BINDING half — honestly small.** The `oauth_provider` field is ALREADY
  Protocol-writable end to end: `AgentConfigMCPConnectionDescriptor.oauth_provider`
  (`internal/protocol/types/agentconfig.go:115`) →
  `AttachRequest.OAuthProvider` → `config.MCPServerConfig.OAuthProvider` →
  `resolveOAuthBinding`. The gap is entirely Console-side: `addConnection()`
  (`web/console/src/lib/agentconfig/state.svelte.ts:912-923`) builds the
  descriptor with `{name, transport, command, url}` and **silently drops
  `oauth_provider`**. This phase adds the field to the Add-connection card as a
  SELECT populated from the installed provider list, threads it into the
  descriptor, and pins the round-trip with a test. No Go wire change is needed
  for the binding itself — say so plainly rather than inventing work.

- **Changing an existing connection's binding is remove + re-add — stated, not
  hidden.** The transport holds the provider reference from attach time, so
  there is no live-patchable binding; a patch verb would be a silent half-write
  (the same reasoning that keeps Phase 166's verb narrow). Documented in the
  method godoc AND the Console copy.

- **Authorization is server-derived and `admin`-gated (hard constraint 4).**
  Both new routes inherit the `AgentConfigHandler` scope gate's `default:` arm
  (`internal/protocol/transports/stream/agentconfig_handler.go:133-155`) by NOT
  being added to the session-safe (`:224-230`) or user (`:238-244`) exception
  maps: `auth.HasScope(ctx, auth.ScopeAdmin)`. Identity comes from
  `resolveIdentity(r)` / `identityFromScope` — never from the request body
  (D-219). No new scope is minted; the scope set stays CLOSED (D-284).

## Non-goals

- **The interactive `oauth2` driver is NOT Protocol-installable.** It requires a
  client secret (env-named) and a browser redirect. Config-only. See the
  departure note.
- **No env-named local-secret provider over the wire, ever** — including a
  "trusted admin" escape hatch. A carve-out here would re-open the exfiltration
  primitive the whole phase exists to close.
- **No credential VALUE over the wire.** D-271's PULL posture is unchanged:
  Harbor never accepts a pushed credential (that is §7 credential passthrough).
  The install carries a broker NAME; the credential is still pulled from the
  coordinator at first need.
- **No catalog-builder widening.** `tools.entries[]` OAuth bindings stay
  boot-declared / restart-required. Only the MCP attach path consults the live
  registry.
- **No live binding patch** on an attached connection (remove + re-add).
- **No discovery-allowance work** — Phase 166 owns it.
- **No provider auto-install from discovered metadata.** D-297's "report, don't
  follow" boundary is absolute: a discovered `oauth_requirement` is a PROPOSAL.
  The Console MAY pre-fill the install form's endpoint fields from it (an
  operator-confirmed copy), but nothing auto-applies. Test-pinned as a negative.

## Acceptance criteria

- [ ] `config.ToolsConfig` gains `OAuthCredentialBrokers []ToolOAuthCredentialBrokerConfig`
      (`{name, url, auth_token_env, cache_ttl?, timeout?}` — the existing
      `ToolOAuthRemoteConfig` shape plus a name); validated at boot (unique
      names, https URL or loopback, non-empty `auth_token_env`); documented in
      `examples/`; restart-required; existing inline `oauth_providers[].remote`
      blocks remain valid (backward-compatible).
- [ ] `agentcfg.ConfigPayload` gains an `OAuthProviders` section of NON-SECRET
      provider descriptors; it round-trips through `SetRevision` / `Active` /
      `Diff` / `Rollback` on all three StateStore drivers and passes the
      `rebuild_completeness_test.go` reflection walk (D-283 — every sibling
      setter carries the new section forward).
- [ ] **Hard constraint 1, test-pinned:** a `set_oauth_provider` write carrying
      `client_id_env`, `client_secret_env`, or `remote.auth_token_env` (at any
      nesting) is REJECTED with a typed `CodeInvalidRequest` naming the field —
      never silently ignored, never stripped-and-accepted; nothing is written to
      the revision. Test: `TestSetOAuthProvider_RejectsEnvNamedSecretFields`
      (table over all three fields + a nested-remote case).
- [ ] A write whose `driver` is anything other than `tokenexchange`, or whose
      `credential_source` is anything other than `remote`, is rejected loudly
      (the Protocol-writable shape is exactly one shape).
- [ ] `credential_broker` resolves against the boot-declared broker set; an
      unknown name fails loud with an error LISTING the declared names (the §4.4
      factory error posture).
- [ ] A provider name that COLLIDES with a boot-declared `tools.oauth_providers[]`
      entry is rejected with a distinct typed error (boot wins; a runtime install
      never shadows a config-declared provider — the Phase 156 boot-declared
      precedent).
- [ ] `auth.ProviderRegistry` exists (interface + one internally-synchronised
      concrete, D-025), is seeded at boot from `auth.BuildProviders`, and is what
      `MCPConnectionAttacher` / `mcpdrv.AttachDeps` consult — so a provider
      installed over the Protocol is bindable by a connection added moments
      later, with NO restart. Test: install → add connection bound to it →
      the bearer is injected on the identity-stamped call.
- [ ] **INSTALL/UNINSTALL symmetry (hard constraint 3):**
      `agent_config.remove_oauth_provider` writes the revision AND `Uninstall`s
      live, CLOSING the provider. A connection still bound to it fails its next
      identity-stamped call LOUD with a typed error — never an unauthenticated
      fallback dial. Test:
      `TestRemoveOAuthProvider_BoundConnectionFailsLoudNotUnauthenticated`.
- [ ] Rolling the agent config back past an install runs the SAME `Uninstall`
      through the run-start reconcile seam (one mechanism, §13) — not a second
      teardown path. Test-pinned.
- [ ] Both verbs are **admin-gated** and the gate is test-pinned (no-scope →
      `CodeScopeMismatch`; `agent_config:user` scope → `CodeScopeMismatch`);
      authority is read from the verified ctx, never the body (D-219).
- [ ] Both verbs emit ONE admin-scope audit event each (non-secret payload:
      agent id, provider name, driver, broker name, endpoint URLs). **A failed
      audit emit fails the CALL closed** (the `handleSetRawHTMLTrust` posture,
      `internal/protocol/mcp.go:860-868`).
- [ ] **No secret in any persisted artifact.** The revision payload, the diff,
      the audit event, and the Protocol response carry NO env-var name, NO
      client secret, NO broker bearer. Pinned by a sentinel-redaction test that
      seeds a recognisable secret in the process env + the broker response and
      asserts it appears in NONE of them (§7).
- [ ] Both methods are registered in EVERY canonical home (`methods.go` const +
      `canonicalMethods` + `canonicalAgentConfigMethods` +
      `canonicalAgentConfigAdminMethods`, `singlesource.go`, `conformance.go`,
      `cmd/harbor-gen-protocol-docs/methods.go`) and
      `make protocol-ts-gen` + `make protocol-docs-gen` are re-run with the
      regenerated artifacts committed (D-223 / D-209 — all three lockstep gates
      green). `ProtocolVersion` unbumped (additive).
- [ ] **The binding no longer drops (D-062 consumer):** the Console
      Add-connection card carries an `oauth_provider` SELECT populated from the
      installed provider list; `addConnection()` threads it into the descriptor.
      A Console vitest asserts the field reaches the client call (the exact
      regression: `state.svelte.ts:912-923` drops it today), and a Go test pins
      the wire round-trip descriptor → attach → `resolveOAuthBinding`.
- [ ] The Console provider card renders the installed providers (name, driver,
      broker, endpoints — no secret surface anywhere) with install + remove
      affordances, and the remove confirmation states the live consequence
      (bound connections break until removed / re-added).
- [ ] `scripts/smoke/phase-167.sh` OK ≥ 3, FAIL = 0.
- [ ] `-race` green; coverage ≥ the stated target on every touched Go package.

## Files added or changed

- `internal/config/config.go` — `ToolsConfig.OAuthCredentialBrokers` +
  `ToolOAuthCredentialBrokerConfig` (the boot-declared, config-only broker; the
  ONE place `auth_token_env` is named).
- `internal/config/validate.go` — broker validation (unique names, https/loopback
  URL, non-empty `auth_token_env`) + the provider→broker name resolution check.
- `internal/tools/auth/registry.go` (new) — the `ProviderRegistry` §4.4 seam
  (interface + one internally-synchronised concrete; D-025).
- `internal/tools/auth/build_providers.go` — seed the registry at boot; resolve a
  provider's `credential_broker` name to its `ToolOAuthRemoteConfig` (the SAME
  `remote` credential source, D-285 — not a fork).
- `internal/tools/drivers/mcp/attach.go` — `AttachDeps.OAuthProviders` becomes the
  registry lookup (not a by-value map); `resolveOAuthBinding` consults it.
- `internal/runtime/serve/mcp_attacher.go` — holds the registry, not a snapshot map.
- `internal/agentcfg/agentcfg.go` — the `OAuthProviders` `ConfigPayload` section +
  the non-secret `OAuthProviderDescriptor`.
- `internal/agentcfg/drivers/statestore/statestore.go` — the section's diff arm.
- `internal/runtime/agentcfg/protocol/setoauthprovider.go` +
  `removeoauthprovider.go` (new) — validate (incl. the secret-bearing-field
  rejection) → `lockAgent` → revision write (siblings carried forward) → live
  `Install` / `Uninstall` → audit emit (fail-closed).
- `internal/runtime/agentcfg/projection/projection.go` — the provider reconcile
  leg beside `ReconcileConnections` (`:141`) so rollback uninstalls through the
  SAME mechanism.
- `internal/protocol/methods/methods.go` — the two method consts + the three
  canonical sets.
- `internal/protocol/types/agentconfig.go` — the request/response wire types +
  the non-secret provider view type.
- `internal/protocol/transports/stream/agentconfig_handler.go` — the two routes
  (admin-gated by inheriting the `default:` arm — NOT added to either exception
  map), reusing `decode` / `assertIdentity` / `writeServiceError`.
- `internal/protocol/singlesource/singlesource.go`,
  `internal/protocol/conformance/conformance.go`,
  `cmd/harbor-gen-protocol-docs/methods.go` + the `typeindex.go` files.
- `web/console/src/lib/protocol/` (wire mirror + client methods),
  `web/console/src/lib/agentconfig/state.svelte.ts` (the provider card state +
  **the `oauth_provider` drop fix at `:912-923`**), the Agent Config provider
  card + the Add-connection binding SELECT.
- `wire-manifest.gen.json` + `docs/site/protocol/{methods,types}.md` (regenerated).
- `examples/` — the credential-broker config block.
- `test/integration/phase167_oauth_provider_install_test.go` (new).
- `docs/plans/phase-92k-*` / the 92-band pointer note (the sibling
  reconciliation — see Risks).
- `scripts/smoke/phase-167.sh` (new); `docs/plans/README.md`;
  `docs/decisions.md` (D-301); `docs/glossary.md`.

## Public API surface

```go
// Wire (additive, both admin-gated):
//   agent_config.set_oauth_provider
//     req  {agent_id, provider: {name, driver:"tokenexchange",
//                                credential_source:"remote",
//                                credential_broker, token_url, auth_url?, scopes[]}}
//     resp {revision, name}
//     REJECTS client_id_env / client_secret_env / remote.auth_token_env — typed, loud.
//   agent_config.remove_oauth_provider
//     req  {agent_id, name}
//     resp {revision, name, uninstalled bool}

// internal/tools/auth — the §4.4 provider seam. One concrete, internally
// synchronised (D-025). Boot seeds it from BuildProviders; the Protocol
// install adds to it; Uninstall CLOSES the provider so a still-bound
// connection's next call fails LOUD (never an unauthenticated fallback).
type ProviderRegistry interface {
    Get(name string) (OAuthProvider, bool)
    Names() []string
    Install(name string, p OAuthProvider) error
    Uninstall(ctx context.Context, name string) error
}
```

## Test plan

- **Unit:**
  - **The secret-bearing-field rejection table** (`client_id_env`,
    `client_secret_env`, `remote.auth_token_env`, nested variants) — typed error,
    nothing written. This is the phase's central security test.
  - Driver / credential-source shape enforcement (anything but
    `tokenexchange` + `remote` is refused).
  - Broker name resolution: unknown → loud error listing declared names.
  - Boot-declared provider name collision → distinct typed error.
  - Scope gate: no-scope and `agent_config:user` → `CodeScopeMismatch`;
    authority never read from the body.
  - Audit fail-closed on a forced emit failure (both verbs).
  - Sentinel-redaction: a recognisable secret seeded in the process env and in
    the broker's response appears in NO revision / diff / event / response.
  - `ProviderRegistry` semantics: install / get / names / uninstall-closes;
    uninstall of an unknown name is a typed error, not a silent no-op.
  - The `no auto-install from discovered metadata` negative (a recorded
    `oauth_requirement` never writes a provider).
- **Integration (`test/integration/phase167_oauth_provider_install_test.go`) —
  binding per §17.1 (this phase consumes agentcfg + tools/auth + tools/mcp +
  protocol + config, all shipped):** real drivers end to end —
  (1) boot with a declared credential broker + a fixture coordinator serving the
  broker credential and a fixture token endpoint;
  (2) `set_oauth_provider` over the Protocol → the registry carries it live;
  (3) `add_mcp_connection` bound to it (the binding field, not dropped) → the
  identity-stamped call to the fixture MCP server carries
  `Authorization: Bearer <exchanged>` — the whole point of the phase, proven on
  the wire;
  (4) `remove_oauth_provider` → the bound connection's next call fails LOUD with
  a typed error, and **not** with an unauthenticated request (asserted against a
  recording fixture: zero credential-less requests reach the server);
  (5) rollback past the install runs the SAME uninstall;
  (6) **identity propagation** through the triple, and a cross-tenant write
  refusal (≥1 failure mode);
  (7) `-race`.
- **Conformance (§17.8):** the token-exchange leg's fixture derives from the
  REAL spec — RFC 8693 (token exchange) request/response shapes and, where the
  broker-pull format is Harbor's own, a captured transcript from the fixture
  coordinator committed with a provenance comment. NOT a hand-authored blob
  encoding the implementer's reading: a wrong-field mutation of the fixture must
  FAIL the test (the right-field/wrong-field discriminator). Reuse the shipped
  Phase 142/154 fixtures where they already carry the shape.
- **Concurrency / leak:** N≥100 concurrent `Install` / `Uninstall` / `Get`
  against ONE shared `ProviderRegistry` interleaved with concurrent attaches
  under `-race` — no torn map, no use-after-close panic (a `Get` racing an
  `Uninstall` returns either the provider or `false`, never a half-closed
  handle), no goroutine leak after teardown (D-025).

## Console consistency

Binding per CLAUDE.md §4.5 item 12 — cites `docs/design/console/CONVENTIONS.md`
(D-121) and `docs/design/console/PAGE-POLISH-PROCEDURE.md`. The work lands on
the EXISTING `/agent-config` page (no new route): the ONE `(console)` route group
with unprefixed URLs (§1), the shared app shell (§2), the
`web/console/src/lib/components/ui/` inventory (no hand-rolled primitives — the
provider list is a `ui/` table, the binding a `ui/` select), the four-state
`<PageState>` async contract, the unified `HarborClient` (no hand-rolled `fetch`
— §4.5 item 5), and `tokens.css` only (stylelint rejects raw literals).

- **The provider card lives beside the connections card on `/agent-config`** —
  the same revisioned surface, so install / remove sit next to the diff and
  rollback affordances that make them honest. It renders name / driver / broker
  / endpoints and NEVER a secret field (there is no secret to render — that is
  the design).
- **The Add-connection card's binding SELECT** is populated from the installed
  provider list; picking one threads `oauth_provider` into the descriptor,
  fixing the silent drop at `state.svelte.ts:912-923`.
- **Remove confirmation states the live consequence in plain words** — bound
  connections' calls fail until they are removed or re-added. No euphemism; the
  §13 no-silent-degradation rule applies to copy as much as to code.
- **The discovered-requirement → install PRE-FILL is operator-confirmed.** The
  MCP Connections rail's discovered `oauth_requirement` (D-297) may deep-link to
  the install form with the endpoint fields PRE-FILLED — never auto-applied. The
  form is submitted by a human; the copy marks the values "unverified — from the
  connected server," matching D-297's report-don't-follow boundary.

## Smoke script additions

`scripts/smoke/phase-167.sh` — classification `live-server` (the verbs are
served by the booted dev stack), with a `unit-tests` companion for the
secret-field-rejection leg:

- `agent_config.set_oauth_provider` and `agent_config.remove_oauth_provider` are
  present in the booted method surface (404/405/501 → SKIP on pre-167 builds).
- A `set_oauth_provider` carrying `client_secret_env` is REJECTED with the typed
  loud error — the security invariant, asserted over the wire.
- A `set_oauth_provider` naming an UNKNOWN credential broker is rejected with a
  loud error.
- A call WITHOUT the admin scope is rejected with `CodeScopeMismatch`.
- Static: both methods appear in `wire-manifest.gen.json` and in the regenerated
  `docs/site/protocol/methods.md` (the D-223 trip-wire).
- `go test -race` the `internal/tools/auth` registry + the
  secret-field-rejection package.
- Done-definition: `OK ≥ 3`, `FAIL = 0`.

## Coverage target

- `internal/tools/auth` (the `ProviderRegistry` seam + broker resolution): 85%
- `internal/runtime/agentcfg/protocol` (the two service methods): 85%
- `internal/agentcfg` (the section + diff arm): 85%
- `internal/config` (the broker config + validation): 90% (existing target)
- `internal/tools/drivers/mcp` (the registry-lookup swap): existing target
  maintained
- Console: vitest on the provider-card state fold + the binding-not-dropped
  regression.

## Dependencies

- **142** (D-271 — the `tokenexchange` PULL driver, the ONLY driver the
  Protocol-writable shape admits).
- **154** (D-285 — the credential-source seam; the broker binding is the same
  `remote` source reached by name).
- **148** (D-278 — the southbound `oauth_provider` binding this phase makes
  reachable).
- **92f** (the `add_mcp_connection` verb + the `ConnectionAttacher` seam),
  **92h** (the Console Agent Config panel).
- **152** (D-283 — the rebuild-completeness guard the new section must satisfy).
- **166** (the v1.14 sibling: it lands the connections EDITOR surface on
  `/agent-config` and the shared write patterns — audit fail-closed, the
  boot-declared refusal, the admin-gate-by-omission — that this phase reuses).
- **118** (D-223 — the TS/docs lockstep gates).
- Related, NOT a dep: the parked **92k** (`auth.Provider` runtime config
  registration seam). See Risks.

## Risks / open questions

- **This phase makes an auth-configuration surface runtime-writable.** That is
  the whole risk, and hard constraint 1 is the whole mitigation. The adversarial
  review must attack the write path first: try to smuggle an env-var name
  through a nested `remote` block, through an unknown extra field that a lax
  decoder tolerates, through a `driver: oauth2` descriptor, through a
  `credential_broker` name that path-traverses, and through a body-supplied
  `tenant` / `admin` claim (D-219). The decoder MUST be strict (unknown fields
  rejected) — a permissive decoder that silently drops `client_secret_env`
  instead of erroring would technically be safe today and become unsafe the
  moment someone adds a field.
- **Sibling reconciliation with the parked 92k (§13 — one mechanism, N
  consumers).** Phase 92k reserves "the `auth.Provider` runtime config
  registration seam" for the PER-SOURCE `OAuthConfig` a runtime-added connection
  needs for the INTERACTIVE flow. This phase builds the registry for the
  PROVIDER LIST (the non-interactive PULL shape). They are different objects but
  adjacent enough to collide: an unparked 92k must REUSE this phase's
  `ProviderRegistry` and add only its per-source config-registration leg — never
  grow a second provider registry. A pointer note lands in `phase-92k`'s plan in
  this PR, and D-301 records the boundary. **This directly contradicts the
  coordinator's "167 is smaller than it looks" framing — the BINDING half is
  indeed small (a Console fix + a round-trip pin), but the provider-descriptor
  INSTALL requires the 92k-shaped registry seam, which is real work. Sized
  honestly as L.**
- **Uninstall's live effect is deliberately breaking.** A bound connection's
  calls fail loud after its provider is uninstalled. That is correct (§13: no
  silent degradation to an unauthenticated dial) but it IS an operator footgun,
  so it is stated in the method godoc, the Console confirmation copy, and the
  audit event. The alternative — refusing to uninstall while a connection is
  bound — was considered and rejected: it would make the revoke path
  un-exercisable exactly when it matters (a leaked broker credential), which is
  the D-287 asymmetry in a new costume.
- **The catalog builder keeps its boot map.** A runtime-installed provider
  cannot be bound by a boot-declared `tools.entries[]` OAuth binding. That is a
  deliberate boundary, not an omission — widening it is a separate ask with a
  larger blast radius (the catalog is built once and its middleware wrapping is
  boot-ordered, D-292). Named here so a future phase does not "discover" it as a
  bug.
- **One broker list vs inline `remote` blocks.** The additive
  `tools.oauth_credential_brokers[]` sits beside the existing inline
  `oauth_providers[].remote` block. Two ways to declare a broker is a mild §13
  smell; it is accepted because the inline form is SHIPPED (D-285,
  backward-compatibility is binding) and the named form is the only one that can
  be referenced by name from a non-secret wire descriptor. The plan does NOT
  deprecate the inline form; if a future phase consolidates, that is an RFC PR.

## Glossary additions

- **Protocol-installed OAuth provider** — an OAuth provider descriptor written
  over the Protocol (`agent_config.set_oauth_provider`, Phase 167 / D-301) onto
  the agent-config revision spine, rather than declared in `harbor.yaml`. Its
  wire shape is restricted to the NON-SECRET broker-pull form
  (`{name, driver: tokenexchange, credential_source: remote, credential_broker,
  token_url, auth_url?, scopes[]}`): a write carrying `client_id_env`,
  `client_secret_env`, or `remote.auth_token_env` is REJECTED loudly, because an
  admin able to name a runtime env var AND a token endpoint owns an env-var
  exfiltration primitive. The process secret is reached by NAME through a
  boot-declared credential broker. Installed providers live in the `auth.ProviderRegistry`
  (§4.4 seam) the MCP attach path consults, so a connection added moments later
  can bind them with no restart; uninstalling CLOSES the provider, so a still-bound
  connection's next call fails LOUD rather than degrading to an unauthenticated
  dial. RFC §6.4, §6.16, D-271, D-285, D-301.
- **Credential broker (named)** — a boot-declared, config/file-only
  `tools.oauth_credential_brokers[]` entry (`{name, url, auth_token_env,
  cache_ttl?, timeout?}`) naming the coordinator endpoint and the env var holding
  the runtime's own service token. It is the ONE place a Protocol-installed
  provider's process secret is named; the provider references it by non-secret
  NAME (the same indirection `mcp.servers[].oauth_provider` uses). Resolves
  through the SAME `remote` credential source as an inline `oauth_providers[].remote`
  block (D-285) — a naming indirection, not a second seam. Phase 167, D-301.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
      (the write + read path identity legs; a cross-tenant write is refused)
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent `Install` /
      `Uninstall` / `Get` against ONE shared `ProviderRegistry` interleaved with
      attaches under `-race`; no torn map, no use-after-close, no goroutine leak
      (D-025)
- [ ] **Integration test exists** (`test/integration/phase167_oauth_provider_install_test.go`),
      wires real drivers end-to-end (fixture coordinator + fixture token endpoint
      + real MCP transport), asserts identity propagation, covers ≥1 failure mode
      (uninstall → loud failure, never an unauthenticated dial), runs under
      `-race`
- [ ] §17.8: the token-exchange fixture derives from RFC 8693 / a captured
      transcript, with a provenance comment; a wrong-field mutation FAILS the test
- [ ] Wire changes complete: `make protocol-ts-gen-check` +
      `make protocol-docs-gen-check` green with regenerated artifacts committed
      (D-223 / D-209); `ProtocolVersion` unbumped (additive)
- [ ] Config schema changed → `examples/` updated; backward compatibility
      verified (inline `oauth_providers[].remote` still loads)
- [ ] §18 skill hygiene: grep `docs/skills/` for `surface: protocol` /
      `surface: console` / `surface: mcp` / `surface: agent-yaml` and update any
      playbook this surface makes stale, in the SAME PR
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry
      filed (D-301 records the RFC 7591 / operator-burden departure)
