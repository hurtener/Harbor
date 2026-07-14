# Phase 169 — Protocol-installed OAuth provider (zero-URL broker-pull shape) + the connection→provider binding

> Part of the v1.14 wave (`docs/plans/wave-v114-coordination.md`). Decision:
> **D-303**. The provider-install half of HA-15, re-homed and DE-SCOPED from the
> original v1.14 Phase 167 after two adversarial reviews restructured the wave.
> It lands ON the credential-sink hardening (166) and the identity-keyed
> registries (167), and after the discovery-allowance write (168).

## Summary

A runtime-added MCP connection can already NAME an OAuth provider — the
non-secret `oauth_provider` binding rides `agentcfg.MCPConnectionDescriptor`
(`internal/agentcfg/agentcfg.go:234`) and `AttachRequest`
(`internal/runtime/agentcfg/protocol/addconnection.go:71-93`) — but the provider
must already exist. This phase makes a provider descriptor installable over the
Protocol on the agent-config revision spine, in the shape that satisfies the
wave's generalized invariant (D-300): **no admin-writable field may determine
where a credential is sent.** Two reviews proved the earlier "non-secret
broker-pull descriptor" still carried `token_url` (an exfil sink for the org's
real `client_id`/`client_secret`) — so the writable shape now carries **ZERO
URLs**: the token endpoint AND the allowed-downstream-host set are pinned at
boot on a named credential broker (Phase 166's `AllowedDownstreamHosts` + the
boot broker's `token_url`). The writable descriptor is reduced to
`{name, credential_broker, scopes?}` — the invariant becomes trivially testable
("no field on the writable descriptor is a URL"). Backing it: an identity-keyed
provider set (Phase 167) the MCP attach path consults; install and uninstall
ship together, uninstall CLOSES the provider so a bound connection's next call
fails LOUD (verified end to end: `tokenexchange.go:360` → `mcp.go:1166-1182`).
The binding half is genuinely small — the wire already carries `oauth_provider`;
the Console silently DROPS it (`state.svelte.ts:912-923`) — this phase stops
dropping it.

## RFC anchor

- RFC §6.4 — the tool catalog + the tool-side OAuth provider seam
  (`internal/tools/auth`) made runtime-registrable.
- RFC §6.16 — the agent-config control plane (the revision spine).
- RFC §5.2 — what the Protocol exposes (the two additive admin verbs + the
  additive config section).
- RFC §6.15 — the governance / audit posture (admin-scope audit on install /
  uninstall; Phase 166's fail-closed ordering).
- RFC §7 — the Console lens (the provider card + the binding SELECT).

## Briefs informing this phase

- brief 09
- brief 14
- brief 11

## Brief findings incorporated

- **brief 09 §"What to lift from bifrost (concrete)" item 2 (RFC 7591 dynamic
  registration):** "implementing it once means operators don't have to
  hand-register a client app per server." The same argument one level up: a
  consumer that brokers credentials centrally should not have to hand-edit yaml
  and restart to give an agent a provider binding. This phase makes the provider
  descriptor installable — in the ZERO-URL shape (see the departure note).
- **brief 09 §"the dynamic-registration footguns":** fail loud, never a silent
  fallback to an unauthenticated dial. Applied to install AND uninstall: an
  unknown broker name, a boot-declared name collision, and a URL-or-secret field
  each fail loud; uninstalling a provider CLOSES it, so a still-bound
  connection's next call fails LOUD (§13 no silent degradation).
- **brief 09 §PKCE / §"What bifrost provides":** the interactive OAuth flow
  needs a redirect, a browser, and a client secret — none of which a
  Protocol-installed descriptor can safely bootstrap. One reason the writable
  shape is restricted to the NON-interactive `tokenexchange` PULL driver (D-271).
- **brief 14 §item 9 ("OAuth for HTTP servers"):** Harbor's MCP auth posture is
  config-declared. This phase does not add a second auth mechanism — it makes
  the EXISTING provider seam (D-083 / D-271 / D-285) reachable from the control
  plane, in the sink-constrained shape Phase 166 established.
- **brief 11 §"MCP Connections view" / §"OAuth & Auth tab":** the Console OAuth
  affordances are pure consumers of the shipped provider flow — never a parallel
  binding-state machine. Held: a provider card + a binding SELECT populated from
  the provider list; no Console-side provider state model.

## Findings I'm departing from (if any)

- **brief 09 is positive on RFC 7591 dynamic registration and on operator-config
  reduction; this phase does NOT make the interactive `oauth2` driver (or any
  env-named local-secret provider) Protocol-installable, and it carries ZERO
  URLs on the writable shape.** The departure is a security judgement two
  adversarial reviews forced: the generalized invariant is "no admin-writable
  field may determine where a credential is sent" (D-300), and `token_url` /
  `auth_url` / any env-var name on a writable descriptor violates it (the org's
  client secret is POSTed to `token_url`; an env-var name + a URL is an
  exfiltration primitive). So every credential-sink-determining value is
  boot-declared on the named broker, and the writable descriptor names only the
  broker + a scope subset. Recorded in D-303; the brief's operator-burden intent
  is still served for the case that motivated HA-15 (the broker-pull provider IS
  installable), just not in a shape that hands an `admin` caller a sink.

## Goals

- **A ZERO-URL, Protocol-writable provider descriptor.** The wire shape is
  exactly `{name, credential_broker, scopes?}` (plus `driver` /
  `credential_source` validated to be exactly `tokenexchange` / `remote`). **No
  URL of any kind, no env-var name, no literal secret.** The invariant is
  structural and trivially testable: a reflective test asserts the wire struct
  exposes NO field whose value is a URL or an env-var name; and because the
  forbidden fields (`token_url`, `auth_url`, `client_id_env`,
  `client_secret_env`, `remote`) are simply NOT on the struct, a decode with
  `DisallowUnknownFields()` rejects any of them by name (`json: unknown field
  "client_secret_env"`) — a loud, field-naming reject with no decoy fields
  (resolving WARN 17's two options in favour of the strongest form: the field
  cannot exist).
- **The named credential broker is the ONE sink authority (built in Phase 166,
  extended here).** `tools.oauth_credential_brokers[]` (Phase 166 added the
  broker's `token_url` + `AllowedDownstreamHosts` + `auth_token_env`; this phase
  relies on it) is boot-declared, config/file-only, never Protocol-writable. An
  installed provider references one by NAME; the broker pins the token endpoint,
  the allowed downstream hosts, and the audience/scope ceiling. An unknown broker
  name fails loud listing the declared set.
- **The identity-keyed provider set (Phase 167 keyed the boot set; this phase
  adds install/uninstall to it).** `auth.ProviderSet` — a NEW type in a NEW file
  `internal/tools/auth/providers.go` (NOT `registry.go`, which is the OAuth
  **driver** registry — WARN 9; a third `Install`/`Uninstall` type in that file
  guarantees driver-vs-instance confusion). It is one interface + one concrete,
  internally-synchronised (D-025), identity-keyed (Phase 167), seeded at boot
  from `BuildProviders`, consulted by the MCP ATTACH path. **It is NOT a §4.4
  driver seam** (WARN 20): §4.4 is interface + `drivers/<name>/` + factory +
  `internal/drivers/prod` blank-import; a provider SET holds instances, not
  drivers, so it has no `drivers/` dir and no `prod` registration — the plan
  says so explicitly so nobody builds a factory with nothing to dispatch. The
  CATALOG builder keeps its boot map (`tools.entries[]` bindings are
  boot-declared / restart-required by design and boot-ordered, D-292; a named
  boundary, not an omission).
- **INSTALL and UNINSTALL ship together (the D-287 lesson).**
  `agent_config.set_oauth_provider` (upsert; a new revision, siblings carried
  forward under D-283) + live `Install`; `agent_config.remove_oauth_provider`
  (drop; a new revision) + live `Uninstall`, which CLOSES the provider
  (`OAuthProvider.Close`, verified `tokenexchange.go:360` → the bound
  connection's next call fails loud at `mcp.go:1166-1182`). Rollback past an
  install runs the SAME uninstall through the run-start reconcile seam
  (`projection.go:141`, identity-scoped by Phase 167) — one mechanism, N
  triggers. **Uninstall is deliberately breaking** and defensible ONLY because
  Phase 167 keys the set: a tenant B run reconciles only ITS OWN declared
  providers against ITS OWN installed set, so B can never uninstall / close A's
  provider (FAIL 6, closed by the dep on 167).
- **A boot-declared provider-name collision is refused** (boot wins; the Phase
  156 precedent), scoped within the caller's triple (Phase 167).
- **The BINDING half — honestly small.** `oauth_provider` is ALREADY
  Protocol-writable end to end; the Console `addConnection()`
  (`state.svelte.ts:912-923`) silently drops it. This phase adds an
  `oauth_provider` SELECT populated from the installed provider list, threads it
  into the descriptor, and pins the round-trip. No Go wire change for the binding
  itself.
- **Authorization is server-derived and `admin`-gated (D-219).** Both routes
  inherit the `AgentConfigHandler` `default:` arm by omission from the exception
  maps; identity from `resolveIdentity(r)` / `identityFromScope`; no new scope
  (D-284). Audit uses Phase 166's corrected fail-closed ordering.

## Non-goals

- **The interactive `oauth2` driver is NOT Protocol-installable** (env-named
  client secret + browser redirect; config-only).
- **No URL or env-var name over the wire, ever** — including a "trusted admin"
  escape hatch (it would re-open the exfil primitive the wave exists to close).
- **No credential VALUE over the wire** (D-271 PULL unchanged; a pushed
  credential is §7 passthrough).
- **No catalog-builder widening** (`tools.entries[]` bindings stay
  boot-declared).
- **No live binding patch** (remove + re-add — the transport holds the provider
  reference from attach time).
- **No provider auto-install from discovered metadata.** D-297's
  report-don't-follow is absolute: a discovered `oauth_requirement` is a
  PROPOSAL. The Console MAY pre-fill the install form's SCOPE field from it
  (operator-confirmed), never the sink — and there is no URL field to pre-fill
  anyway. Test-pinned negative.

## Acceptance criteria

- [ ] The writable provider descriptor carries `{name, credential_broker,
      scopes?}` (+ `driver` / `credential_source` validated to exactly
      `tokenexchange` / `remote`). A reflective test asserts the wire struct
      exposes NO URL-typed or env-var-name field ("no field on the writable
      descriptor is a URL"). Test: `TestSetOAuthProviderWire_HasNoSinkField`.
- [ ] A write carrying `token_url`, `auth_url`, `client_id_env`,
      `client_secret_env`, or `remote` is REJECTED — a `DisallowUnknownFields`
      decode error NAMING the offending field (`unknown field "…"`), never
      silently ignored, never stripped-and-accepted; nothing is written to the
      revision. Test: `TestSetOAuthProvider_RejectsSinkAndSecretFields` (table
      over all five).
- [ ] `credential_source` absent/empty is a LOUD reject (WARN 16 — in config,
      `""` means the `env` source, `config.go:1096-1101`, which this shape
      forbids). `driver` != `tokenexchange` and `credential_source` != `remote`
      are likewise rejected.
- [ ] `credential_broker` resolves against the boot-declared broker set (Phase
      166); an unknown name fails loud listing the declared names.
- [ ] A provider name colliding with a boot-declared `tools.oauth_providers[]`
      entry (within the caller's triple) is refused with a distinct typed error.
- [ ] `auth.ProviderSet` (NEW `internal/tools/auth/providers.go` — NOT
      `registry.go`) exists: interface + one internally-synchronised concrete
      (D-025), identity-keyed (Phase 167), seeded at boot from `BuildProviders`,
      consulted by the MCP attach path. It is NOT registered in
      `internal/drivers/prod` and has no `drivers/` dir (it holds instances, not
      drivers — WARN 20). Test: install → add connection bound to it → the bearer
      is injected on the identity-stamped call.
- [ ] **INSTALL/UNINSTALL symmetry + cross-tenant safety (FAIL 6):**
      `remove_oauth_provider` writes the revision AND `Uninstall`s live, CLOSING
      the provider; a still-bound connection's next call fails LOUD (never an
      unauthenticated dial); and a tenant-B run's reconcile NEVER closes a
      tenant-A provider (Phase 167 keying). Tests:
      `TestRemoveOAuthProvider_BoundConnectionFailsLoudNotUnauthenticated`,
      `TestReconcile_TenantBRun_NeverUninstallsTenantAProvider`.
- [ ] Rollback past an install runs the SAME uninstall through the run-start
      reconcile seam (one mechanism, §13). Test-pinned.
- [ ] Both verbs are **admin-gated** (no-scope / `agent_config:user` →
      `CodeScopeMismatch`; authority server-derived, D-219), registered in every
      canonical home (+ the `methods.go` prose counts updated, NIT 19), and
      `make protocol-ts-gen` + `make protocol-docs-gen` re-run with regenerated
      artifacts committed (D-223 / D-209). `ProtocolVersion` unbumped.
- [ ] Both verbs emit ONE admin-scope audit event each, failing the CALL closed
      on emit failure with NO observable state change (Phase 166's corrected
      ordering).
- [ ] **No secret / no sink in any persisted artifact.** The revision, diff,
      audit event, and Protocol response carry NO URL, NO env-var name, NO
      client secret, NO broker bearer. Sentinel-redaction test seeds a
      recognisable secret in the process env + the broker response and asserts it
      appears in NONE of them (§7).
- [ ] **The binding no longer drops (D-062 consumer):** the Console
      Add-connection card carries an `oauth_provider` SELECT from the installed
      provider list; `addConnection()` threads it into the descriptor. A vitest
      asserts the field reaches the client call (the `state.svelte.ts:912-923`
      regression); a Go test pins the wire round-trip descriptor → attach →
      `resolveOAuthBinding` (which, per Phase 166, also enforces the downstream
      host).
- [ ] `scripts/smoke/phase-169.sh` OK ≥ 3, FAIL = 0.
- [ ] `-race` green; coverage ≥ the stated target on every touched Go package.

## Files added or changed

- `internal/tools/auth/providers.go` (NEW) — the `ProviderSet` (identity-keyed;
  install/uninstall/get/names). NOT in `registry.go` (WARN 9); no `drivers/prod`
  registration (WARN 20).
- `internal/tools/auth/build_providers.go` — seed the `ProviderSet` at boot;
  resolve a provider's `credential_broker` name to its boot broker (Phase 166).
- `internal/tools/drivers/mcp/attach.go` + `internal/runtime/serve/mcp_attacher.go`
  — consult the identity-keyed `ProviderSet` (not a by-value map).
- `internal/agentcfg/agentcfg.go` — the `OAuthProviders` `ConfigPayload` section
  plus the zero-URL `OAuthProviderDescriptor`;
  `internal/agentcfg/drivers/statestore/statestore.go` — its diff arm.
- `internal/runtime/agentcfg/protocol/setoauthprovider.go` +
  `removeoauthprovider.go` (new) — decode (`DisallowUnknownFields`) → validate →
  `lockAgent` → revision write → live `Install` / `Uninstall` → audit
  (fail-closed helper from Phase 166).
- `internal/runtime/agentcfg/projection/projection.go` — the provider reconcile
  leg (rollback uninstalls through the same mechanism), identity-scoped.
- `internal/protocol/methods/methods.go` — the two method consts + the three
  canonical sets + the prose counts.
- `internal/protocol/types/agentconfig.go` — the request/response types + the
  zero-URL provider view type.
- `internal/protocol/transports/stream/agentconfig_handler.go` — the two routes
  (admin-gated by omission).
- `internal/protocol/singlesource/singlesource.go`,
  `internal/protocol/conformance/conformance.go`,
  `cmd/harbor-gen-protocol-docs/methods.go` + the `typeindex.go` files.
- `web/console/src/lib/protocol/` (wire mirror + client methods),
  `web/console/src/lib/agentconfig/state.svelte.ts` (the provider card + the
  `oauth_provider` drop fix), the Agent Config provider card + the
  Add-connection binding SELECT.
- `wire-manifest.gen.json` + `docs/site/protocol/{methods,types}.md`
  (regenerated).
- `docs/plans/phase-92k-auth-provider-runtime-config.md` +
  `docs/plans/phase-92m-add-connection-oauth-flow.md` — the pointer notes (an
  unparked 92k reuses THIS `ProviderSet`; 92m must not add a parallel add-request
  OAuth block — WARN 13; ACTUALLY written in this PR).
- `test/integration/phase169_oauth_provider_install_test.go` (new).
- `scripts/smoke/phase-169.sh` (new); `docs/plans/README.md`;
  `docs/decisions.md` (D-303); `docs/glossary.md`.

## Public API surface

```go
// Wire (additive, both admin-gated) — ZERO URLs:
//   agent_config.set_oauth_provider
//     req  {agent_id, provider: {name, driver:"tokenexchange",
//                                credential_source:"remote",
//                                credential_broker, scopes?}}
//     resp {revision, name}
//     DisallowUnknownFields REJECTS token_url / auth_url / client_id_env /
//     client_secret_env / remote — by name.
//   agent_config.remove_oauth_provider
//     req  {agent_id, name}
//     resp {revision, name, uninstalled bool}

// internal/tools/auth — NEW providers.go. Identity-keyed (Phase 167),
// internally synchronised (D-025). NOT the driver registry (registry.go);
// NOT a §4.4 driver seam (holds instances, no drivers/prod registration).
type ProviderSet interface {
    Get(id identity.Identity, name string) (OAuthProvider, bool)
    Names(id identity.Identity) []string
    Install(id identity.Identity, name string, p OAuthProvider) error
    Uninstall(ctx context.Context, id identity.Identity, name string) error
}
```

## Test plan

- **Unit:** the sink/secret-field rejection table (`token_url`, `auth_url`,
  `client_id_env`, `client_secret_env`, `remote`) — named decode error, nothing
  written; the no-URL-field reflective structural test; driver / credential_source
  shape enforcement incl. the empty-`credential_source` reject (WARN 16); broker
  name resolution (unknown → loud); boot-declared collision (triple-scoped);
  scope gate; audit fail-closed (no state change); sentinel-redaction; the
  `no-auto-install-from-discovered-metadata` negative; `ProviderSet` semantics
  incl. uninstall-closes and identity-keying.
- **Integration (`test/integration/phase169_oauth_provider_install_test.go`) —
  binding per §17.1:** real drivers end to end — boot with a named credential
  broker (Phase 166) + a fixture coordinator + a fixture token endpoint;
  `set_oauth_provider` → the identity-keyed set carries it; `add_mcp_connection`
  bound to it → the identity-stamped call carries `Authorization: Bearer
  <exchanged>` (the whole point, on the wire, and the downstream host is on the
  broker's allow-list per Phase 166); `remove_oauth_provider` → the bound
  connection's next call fails LOUD (recording fixture: zero credential-less
  requests reach the server); rollback → same uninstall; **cross-tenant:** a
  tenant-B run never closes a tenant-A provider (FAIL 6); identity propagation +
  a cross-tenant write refusal; `-race`.
- **Conformance (§17.8):** the token-exchange leg derives from RFC 8693 / a
  captured coordinator transcript with a provenance comment; a wrong-field
  mutation FAILS. Reuse Phase 142/154/166 fixtures.
- **Concurrency / leak:** N≥100 concurrent `Install` / `Uninstall` / `Get`
  across ≥2 tenants against ONE shared `ProviderSet` interleaved with attaches
  under `-race` — no torn map, no use-after-close (a `Get` racing an `Uninstall`
  returns the provider or `false`, never a half-closed handle), no cross-tenant
  bleed, no goroutine leak (D-025).

## Console consistency

Binding per CLAUDE.md §4.5 item 12 — cites `docs/design/console/CONVENTIONS.md`
(D-121) and `docs/design/console/PAGE-POLISH-PROCEDURE.md`. Lands on the
EXISTING `/agent-config` page (no new route): the ONE `(console)` route group
(§1), the shared app shell (§2), the `ui/` inventory (the provider list is a
`ui/` table, the binding a `ui/` select — no hand-rolled primitives), the
four-state `<PageState>` contract, the unified `HarborClient` (no hand-rolled
`fetch` — §4.5 item 5), and `tokens.css` only.

- **The provider card lives beside the connections card on `/agent-config`** —
  the same revisioned surface, so install / remove sit next to diff + rollback.
  It renders name / broker / scopes and NEVER a URL or secret (there is none to
  render — that is the design).
- **The Add-connection card's binding SELECT** is populated from the installed
  provider list; picking one threads `oauth_provider` into the descriptor,
  fixing the drop at `state.svelte.ts:912-923`.
- **Remove confirmation states the live consequence in plain words** — bound
  connections' calls fail until they are removed or re-added. No euphemism (§13
  applies to copy).
- **Discovered-requirement pre-fill is operator-confirmed and sink-free.** The
  MCP Connections rail's discovered `oauth_requirement` (D-297) may deep-link to
  the install form pre-filling only the SCOPE field (marked "unverified — from
  the connected server"); there is no URL field to pre-fill, and nothing
  auto-applies.

## Smoke script additions

`scripts/smoke/phase-169.sh` — classification `live-server` (the verbs are
served by the booted dev stack), with a `unit-tests` companion for the
sink/secret-field-rejection leg:

- `agent_config.set_oauth_provider` + `agent_config.remove_oauth_provider`
  present on the booted method surface (404/405/501 → SKIP on pre-169 builds).
- A `set_oauth_provider` carrying `token_url` (or `client_secret_env`) is
  REJECTED — the security invariant, over the wire.
- A write naming an UNKNOWN credential broker is rejected loudly.
- A call WITHOUT admin scope → `CodeScopeMismatch`.
- Static: both methods in `wire-manifest.gen.json` + the regenerated
  `docs/site/protocol/methods.md`.
- `go test -race` the `internal/tools/auth` `ProviderSet` + the
  sink/secret-field rejection package.
- Done-definition: `OK ≥ 3`, `FAIL = 0`.

## Coverage target

- `internal/tools/auth` (the `ProviderSet` + broker resolution): 85%
- `internal/runtime/agentcfg/protocol` (the two service methods): 85%
- `internal/agentcfg` (the section + diff arm): 85%
- `internal/tools/drivers/mcp` (the attach-path set lookup): existing target
- Console: vitest on the provider-card state fold + the binding-not-dropped
  regression.

## Dependencies

- **166** (the credential-sink hardening — the named broker carries the
  `token_url` + `AllowedDownstreamHosts` this phase relies on, so the descriptor
  needs no URL; and the corrected audit ordering).
- **167** (the identity-keyed provider set — makes install/uninstall
  cross-tenant-safe, FAIL 4/6).
- **168** (the v1.14 sibling that lands the `/agent-config` editor surface + the
  shared write patterns).
- **142** (D-271 — the only driver the writable shape admits), **154** (D-285 —
  the credential-source seam), **148** (D-278 — the binding this makes
  reachable), **92f** (the add verb + attacher), **92h** (the Console panel),
  **152** (D-283), **118** (D-223).
- Related, NOT a dep: the parked **92k** (`auth.Provider` runtime config
  registration seam — see Risks).

## Risks / open questions

- **This makes an auth-configuration surface runtime-writable.** The whole risk;
  the ZERO-URL structural invariant + the boot-pinned broker are the whole
  mitigation. The adversarial review attacks the write path first: smuggle a URL
  or env-var name through an unknown field (`DisallowUnknownFields` must be on),
  a `driver: oauth2` descriptor, an empty `credential_source`, a path-traversing
  `credential_broker` name, a body-supplied `tenant`/`admin` claim (D-219).
- **Sibling reconciliation with the parked 92k / 92m (§13 — WARN 13).** 92k
  reserves a per-source `OAuthConfig` registration seam for the INTERACTIVE flow;
  92m plans an add-request OAuth block. Both must reuse THIS phase's surfaces —
  the `ProviderSet` for the provider list, this install verb for the binding —
  never grow a parallel provider registry or a parallel add-request auth
  affordance. Pointer notes are ACTUALLY written into `phase-92k` and `phase-92m`
  in this PR (the earlier plan promised this and did not deliver it); D-303
  records the ruling.
- **Uninstall's live effect is deliberately breaking** — correct (§13: no silent
  degradation) and defensible ONLY because Phase 167 keys the set per triple, so
  the break is confined to the owning tenant. Stated in the godoc, the Console
  copy, and the audit event. "Refuse to uninstall while bound" was rejected: it
  makes revoke un-exercisable exactly when a broker credential has leaked (the
  D-287 asymmetry in a new costume).
- **The catalog builder keeps its boot map.** A runtime-installed provider
  cannot be bound by a boot-declared `tools.entries[]` binding — a deliberate
  boundary (D-292), not an omission.
- **`ProviderSet` naming (WARN 9/20).** Named for what it HOLDS (instances), in a
  new file, explicitly NOT the driver registry and NOT a §4.4 driver seam — so no
  one builds a factory with nothing to dispatch, and no call site confuses
  `auth.Register` (a driver) with `ProviderSet.Install` (an instance).

## Glossary additions

- **Protocol-installed OAuth provider** — an OAuth provider descriptor written
  over the Protocol (`agent_config.set_oauth_provider`, Phase 169 / D-303) onto
  the agent-config revision spine. Its wire shape carries ZERO URLs —
  `{name, credential_broker, scopes?}` — because the wave's invariant is that no
  admin-writable field may determine where a credential is sent (D-300): the
  token endpoint, the allowed downstream hosts, and the audience/scope ceiling
  are all pinned at boot on the named credential broker (Phase 166). A write
  carrying any URL or env-var name is rejected by name. Installed providers live
  in the identity-keyed `auth.ProviderSet` (Phase 167) the MCP attach path
  consults; uninstalling CLOSES the provider, so a still-bound connection's next
  call fails LOUD rather than degrading to an unauthenticated dial, and the break
  is confined to the owning tenant by the identity keying. RFC §6.4, §6.16,
  D-271, D-285, D-300, D-303.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] **If multi-isolation code paths changed: cross-tenant isolation test
      passes** — a tenant-B run never closes a tenant-A provider (Phase 167
      keying); a cross-tenant write is refused
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent `Install` /
      `Uninstall` / `Get` across ≥2 tenants against ONE shared `ProviderSet`
      under `-race`; no torn map, no use-after-close, no cross-tenant bleed, no
      goroutine leak (D-025)
- [ ] **Integration test exists**
      (`test/integration/phase169_oauth_provider_install_test.go`), wires real
      drivers end-to-end (named broker + fixture coordinator + fixture token
      endpoint + real MCP transport), asserts identity propagation + the
      uninstall-fails-loud + cross-tenant-safety legs, covers ≥1 failure mode,
      runs under `-race`
- [ ] §17.8: the token-exchange fixture derives from RFC 8693 / a captured
      transcript with a provenance comment; a wrong-field mutation FAILS
- [ ] Wire changes complete: `make protocol-ts-gen-check` +
      `make protocol-docs-gen-check` green with regenerated artifacts committed
      (D-223 / D-209); the `methods.go` prose counts updated (NIT 19);
      `ProtocolVersion` unbumped
- [ ] Config schema: the named-broker `token_url`/`AllowedDownstreamHosts` land
      in Phase 166; this phase adds no new config key beyond referencing them —
      `examples/` shows a Protocol-installed provider selecting a broker
- [ ] The 92k / 92m pointer notes are written (WARN 13)
- [ ] §18 skill hygiene: `docs/skills/use-the-harbor-protocol/SKILL.md`
      (`surface: protocol`) documents the new methods; `define-the-agent-yaml`
      (`surface: agent-yaml`) if the broker example changes — in the SAME PR
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry
      filed (D-303 records the ZERO-URL / RFC 7591 departure)
