# Phase 166 — Credential-sink hardening (shipped-code security fix)

> Opens the v1.14 wave (`docs/plans/wave-v114-coordination.md`). Decision:
> **D-300**. This phase has **no new Protocol surface** — it is a pure security
> fix of SHIPPED code, and the base the rest of the wave (167/168/169) stands
> on. Two adversarial reviews of the original v1.14 shape (env-var-name
> rejection) proved that rule was a symptom rule; this phase implements the
> generalized invariant instead.

## Summary

Harbor's tool-side OAuth already lets an operator bind an MCP connection to a
credential provider (D-278) and pull a brokered token at call time (D-271). A
review of that shipped surface found three ways an `admin`-scoped caller — a
scope that is otherwise configuration-shaped — can turn it into a credential
exfiltration channel, all reachable in shipped Harbor today (92f's
`add_mcp_connection` + D-278's southbound binding), none introduced by HA-15:
(1) the provider's `token_url` is where the runtime POSTs the org's real OAuth
`client_id` + `client_secret` (`internal/tools/auth/drivers/tokenexchange/tokenexchange.go:582`),
so any admin-writable `token_url` is an exfil sink; (2) the CONNECTION `url` is
where the exchanged downstream bearer is injected, and `resolveOAuthBinding`
(`internal/tools/drivers/mcp/attach.go:288-320`) places NO constraint on that
host — while the provider name is the default token audience
(`tokenexchange.go:214-217`), so the caller even picks the audience+scopes of
the token they steal; (3) the token-exchange HTTP client is a bare
`&http.Client{Timeout: 30s}` (`tokenexchange.go:242-245`) with the default
redirect policy, so a `307/308` from the broker re-POSTs the `client_secret`
form to the redirect target (Go replays the body). This phase closes all three
with the generalized invariant **"no admin-writable field may determine where a
credential is sent"**, and fixes a fourth defect the reviews surfaced: the
`handleSetRawHTMLTrust` audit-ordering godoc is a lie (it applies the mutation
THEN emits, but its godoc + error claim it "fails the call closed"), which both
downstream phases were about to copy as their audit posture.

## RFC anchor

- RFC §6.4 — the tool catalog, its transports, and the tool-side OAuth provider
  seam (`internal/tools/auth`, the MCP southbound binding) being hardened.
- RFC §6.15 — the governance / audit posture (the credential ceiling; the
  audit-ordering correction).
- RFC §7 — the security rules this enforces (no credential passthrough by
  default; no unbounded credential egress).

## Briefs informing this phase

- brief 09
- brief 03

## Brief findings incorporated

- **brief 09 §"the dynamic-registration footguns" / §"What to lift from
  bifrost":** OAuth wiring fails loud, never a silent fallback to an
  unauthenticated dial. Applied to every leg here: a binding whose downstream
  host is not on the boot-declared allow-list is REFUSED at attach (loud), never
  attached-and-unauthenticated; a redirecting broker is a fault, not a hop.
- **brief 09 §PKCE / §"the credential never leaves the trust boundary":** the
  credential plane's whole point is that the secret has ONE sink the operator
  controls. A runtime-influenceable sink violates that at the root — the review
  finding this phase closes. The fix keeps every credential-determining value
  (token endpoint, allowed downstream hosts, scope ceiling) boot-declared.
- **brief 03 §"Tools + integrations — the HTTP edge":** an outbound HTTP client
  that carries a secret needs a hardened transport (bounded redirects, no proxy
  surprises, no private-range reachability). Harbor already applies exactly this
  to the discovery client (`internal/tools/auth/discovery.go:260-281`) and to
  the credential-source fetch (`credsource/drivers/remote/remote.go:127-134`,
  which refuses every redirect PRECISELY because it carries a bearer) — this
  phase brings the token-exchange client up to the same bar, citing those two
  in-repo precedents rather than inventing a posture.

## Findings I'm departing from (if any)

None from the briefs.

**Implementation deviation (§4.3) — the hardened token-exchange client
allows LOOPBACK.** D-300 point 3 / the goals list the dial guard as refusing
"private-range / loopback / IP-literal" destinations, mirroring the
discovery client. Once the code landed, that loopback refusal broke the
real-boot-path fixtures (`TestE2E_Phase149`, the Phase 142 broker fixture)
which run their credential broker on a loopback `httptest.Server` through the
production path (`assemble.Assemble` → `BuildProviders` → nil `HTTPClient` →
the hardened client), and — more importantly — it would break the legitimate
production deployment where the credential broker is a **localhost sidecar**
(a token-vault agent on `127.0.0.1`). The token endpoint here is
BOOT-DECLARED / config-only (never wire-derived), so it is not the
attacker-influenceable input the discovery client's loopback refusal defends
against. The dial guard therefore refuses private-range / link-local / RFC1918
/ unique-local space (the DNS-rebinding backstop stays) but ALLOWS loopback —
exactly the carve-out the cited in-repo precedent `credsource/drivers/remote`
already makes for its own bearer-carrying client (loopback `http` accepted).
The AC test `TestTokenExchange_HTTPClient_RefusesPrivateDial` asserts refusal
on a non-loopback RFC1918 address; a new `..._AllowsLoopback` pins the
carve-out. This is a §17.6 fix of what the integration suite surfaced.

## Goals

- **The generalized invariant (binding, D-300): no admin-writable field may
  determine where a credential is sent.** This supersedes the original v1.14
  "no env-var NAMES on the wire" rule, which two reviews proved insufficient
  (renaming the broker via a named indirection only renamed the sink; it did
  not remove it — the `token_url` and connection `url` remained
  admin-writable). The invariant is enforced structurally across this wave;
  this phase enforces its shipped-code half.
- **Boot-declared downstream-host allow-list (closes FAIL 2).**
  `ToolOAuthProviderConfig` gains `AllowedDownstreamHosts []string` — a
  boot-declared, config/file-only set of host[:port] values a provider may
  inject its bearer into. `resolveOAuthBinding` REFUSES a binding whose
  connection host (`ms.URL`'s host) is not in the provider's allow-list — loud,
  typed, never a silent unauthenticated dial. Covers EVERY provider driver
  (the interactive `oauth2` southbound binding is exfil-reachable too, D-278),
  not just `tokenexchange`. An empty allow-list on a provider that any
  connection binds is a boot-validation error (fail-closed: a provider that can
  inject a bearer must say where).
- **The boot credential-broker list — the config home for the pinned sinks
  (closes WARN-C; the base Phase 169 references).** Config gains
  `tools.oauth_credential_brokers[]` — a NAMED, boot-declared, config/file-only
  list whose entry is `{name, token_url, allowed_downstream_hosts,
  auth_token_env, cache_ttl?, timeout?}`. It is the boot home for the credential
  SINKS Phase 169's zero-URL Protocol-installed provider references BY NAME: the
  token endpoint and the allowed-downstream-host set live here, never on a
  wire-writable descriptor. This belongs in THIS phase because it IS the
  credential-sink hardening — the whole point of D-300 is that every sink is
  boot-pinned. Boot validation: unique names; https `token_url` (or loopback);
  a non-empty `allowed_downstream_hosts`; a non-empty `auth_token_env`. The
  existing inline `oauth_providers[].remote` block stays valid
  (backward-compatible; the D-285 credential-source seam is reached by name, not
  forked). Config-only, restart-required — NOT a Protocol surface.
- **Scope + audience ceiling (closes the audience-picking half of FAIL 2).**
  The token audience is NOT derived from the caller-chosen provider name: a
  boot-declared `Audience` (and `Scopes` ceiling) on the provider/broker is the
  authority, and any requested scope set is INTERSECTED against the ceiling
  (a requested scope outside the ceiling is dropped, not honoured). The
  `tokenexchange` provider stops defaulting `audience = cfg.Name`
  (`tokenexchange.go:214-217`) when a ceiling is declared; the ceiling wins.
- **Harden the token-exchange HTTP client (closes FAIL 3).** Replace the bare
  `&http.Client{Timeout: 30s}` (`tokenexchange.go:242-245`) with the discovery
  client's treatment: a `net.Dialer.Control` hook refusing private-range /
  loopback / IP-literal destinations POST-DNS-RESOLUTION
  (`discovery.go:260-281`), `Proxy: nil`, and a `CheckRedirect` that REFUSES
  every redirect with a typed sentinel (the `credsource/remote` precedent,
  `remote.go:127-134` — a client carrying a `client_secret` must not replay it
  to a redirect target). A caller-supplied `deps.HTTPClient` is shallow-copied
  and re-hardened, never mutated in place (the `credsource/remote` pattern).
- **Harden the MCP bearer client's redirects (closes WARN-D — the exchanged
  downstream bearer can egress via a redirect).** The token-exchange fix above
  protects the org's `client_secret`; a SEPARATE hole leaks the EXCHANGED
  downstream token. `bearerInjectingTransport` re-injects
  `Authorization: Bearer <exchanged>` on EVERY hop from inside the RoundTripper
  (`internal/tools/drivers/mcp/transport_sse.go:100-111`), and the MCP bearer
  client is built on `http.DefaultClient` / a bare `&http.Client{Transport: rt}`
  with the DEFAULT redirect policy (`transport_sse.go:55-61`). Go's cross-host
  header stripping does NOT help — the injection is in the RoundTripper, not
  `req.Header` — so an allow-listed host that answers `302` sends the exchanged
  bearer to an arbitrary redirect target. Fix: the MCP bearer client gets a
  `CheckRedirect` that RE-VALIDATES the redirect target host against the bound
  provider's `AllowedDownstreamHosts` (a redirect to an unlisted host is
  refused with a typed sentinel — same discipline as the token-exchange client).
  Named AC + test.
- **Fix the audit-ordering lie (a WARN promoted to in-scope, §17.6).**
  `handleSetRawHTMLTrust` (`internal/protocol/mcp.go:843-875`) godoc claims "a
  failed audit emit fails the call closed — an un-auditable trust toggle is
  refused", but the code APPLIES the mutation (`SetRawHTMLTrust`) and THEN
  emits, and on emit failure returns "trust toggle applied but audit emit
  failed" — the mutation already happened. Phases 168/169 were about to cite
  this as the audit posture to copy, making their audit ACs unsatisfiable. Fix
  it to a genuinely fail-closed ordering — **emit-then-apply** where the apply
  cannot fail after a successful emit, or **apply-then-emit-then-COMPENSATE**
  (revert the mutation on emit failure) — and correct the godoc to describe what
  the code actually does. This establishes the ONE audit posture 168/169 copy.

## Non-goals

- **No new Protocol method, wire type, or event.** This phase is config-schema +
  internal hardening only. The audit-ordering fix touches an existing handler's
  ordering, not its wire shape.
- **No identity-keying of the registries** — that is Phase 167. This phase's
  allow-list and ceiling are boot-declared per provider, which is correct under
  either tenancy topology and does not depend on 167.
- **No discovery-allowance write** (Phase 168) and **no provider install**
  (Phase 169) — both stand ON this phase's hardening.
- **No removal of the interactive `oauth2` driver's config surface.** It stays
  config-declared; this phase only bounds where its bearer may be injected.
- **No change to D-271's PULL posture** — the credential is still pulled at
  call time and never persisted; this phase bounds the SINK, not the flow.

## Acceptance criteria

- [ ] `ToolOAuthProviderConfig` carries `AllowedDownstreamHosts []string`
      (boot-declared, config/file-only, documented in `examples/`,
      restart-required). Boot validation REJECTS a provider that any connection
      can bind with an empty allow-list (fail-closed — a bearer-injecting
      provider must declare its sinks).
- [ ] `resolveOAuthBinding` refuses a binding whose connection host is not in
      the bound provider's `AllowedDownstreamHosts`, with a distinct typed error
      — never a silent unauthenticated dial. Host comparison normalises
      host[:port] with the same normaliser the allow-list validation uses (one
      normaliser). Test:
      `TestResolveOAuthBinding_RefusesUnlistedDownstreamHost` (fails-without /
      passes-with).
- [ ] The token audience is the boot-declared ceiling, NOT the caller-chosen
      provider name, whenever a ceiling is declared; requested scopes are
      INTERSECTED against the boot ceiling (an out-of-ceiling scope is dropped).
      Tests: `TestTokenExchange_AudienceFromCeilingNotProviderName`,
      `TestTokenExchange_ScopesIntersectedAgainstCeiling`.
- [ ] The token-exchange HTTP client refuses a private-range / loopback /
      IP-literal destination at DIAL time (post-DNS `net.Dialer.Control`),
      disables the proxy, and REFUSES every redirect with a typed sentinel — the
      `client_secret` form is never replayed to a redirect target. A
      caller-supplied client is shallow-copied and re-hardened, never mutated.
      Tests: `TestTokenExchange_HTTPClient_RefusesPrivateDial`,
      `TestTokenExchange_HTTPClient_RefusesRedirect`.
- [ ] **The MCP bearer client re-validates redirect targets (WARN-D):** a
      redirect to a host not in the bound provider's `AllowedDownstreamHosts` is
      refused with a typed sentinel, so the exchanged downstream bearer
      (`bearerInjectingTransport`, `transport_sse.go:100-111`) is never sent to
      an arbitrary redirect target. Test:
      `TestMCPBearerClient_RefusesRedirectToUnlistedHost` (an allow-listed host
      that 302s to an unlisted host is refused).
- [ ] `tools.oauth_credential_brokers[]` exists (boot-declared, config/file-only,
      restart-required): `{name, token_url, allowed_downstream_hosts,
      auth_token_env, cache_ttl?, timeout?}`; boot validation rejects duplicate
      names, a non-https/non-loopback `token_url`, an empty
      `allowed_downstream_hosts`, and an empty `auth_token_env`; documented in
      `examples/`; the inline `oauth_providers[].remote` block still loads
      (backward-compatible). This is the boot home Phase 169's zero-URL
      descriptor references by name (WARN-C — both plans agree on what exists).
- [ ] `handleSetRawHTMLTrust` is genuinely fail-closed: on audit-emit failure
      the trust toggle is NOT observably applied (emit-then-apply, or
      apply-then-emit-then-revert). The godoc is corrected to describe the
      actual ordering. Test:
      `TestSetRawHTMLTrust_AuditEmitFailure_LeavesTrustUnchanged`. This
      establishes the audit posture Phases 168/169 cite.
- [ ] The three exfil paths are pinned by a security test class that fails
      against the PRE-fix code and passes after — the discriminator lives
      entirely within this phase's own code (no cross-phase dependency).
- [ ] `scripts/smoke/phase-166.sh` OK ≥ 2, FAIL = 0.
- [ ] `-race` green; coverage ≥ the stated target on every touched Go package.

## Files added or changed

- `internal/config/config.go` — `ToolOAuthProviderConfig.AllowedDownstreamHosts`
  (if not already boot-declarable) plus an `Audience` / `Scopes` ceiling surface,
  and the new `ToolsConfig.OAuthCredentialBrokers []ToolOAuthCredentialBrokerConfig`
  (`{name, token_url, allowed_downstream_hosts, auth_token_env, cache_ttl?,
  timeout?}` — the boot home for the pinned sinks, WARN-C);
  `internal/config/validate.go` — the allow-list + ceiling + broker validation
  (fail-closed on an empty allow-list for a bindable provider; unique broker
  names; https/loopback `token_url`; a non-empty broker
  `allowed_downstream_hosts`; a non-empty `auth_token_env`).
- `internal/tools/drivers/mcp/attach.go` — `resolveOAuthBinding` gains the
  downstream-host check (against the resolved provider's allow-list).
- `internal/tools/drivers/mcp/transport_sse.go` — the MCP bearer client's
  `CheckRedirect` re-validating the redirect target against
  `AllowedDownstreamHosts` (WARN-D).
- `internal/tools/auth/registry.go` (`ProviderConfig`) + the
  `internal/tools/auth/drivers/tokenexchange/tokenexchange.go` construction —
  the ceiling wiring + the hardened HTTP client (dial guard, no proxy, refuse
  redirects, shallow-copy a caller client).
- `internal/protocol/mcp.go` — the `handleSetRawHTMLTrust` audit-ordering fix +
  corrected godoc.
- `examples/` — the `allowed_downstream_hosts` + ceiling + `oauth_credential_brokers`
  config block.
- `test/integration/phase166_credential_sink_test.go` (new).
- `scripts/smoke/phase-166.sh` (new); `docs/plans/README.md`;
  `docs/decisions.md` (D-300); `docs/glossary.md`.

## Public API surface

```go
// Config (additive, boot-declared, restart-required):
//   ToolOAuthProviderConfig.AllowedDownstreamHosts []string  // required non-empty when bindable
//   ToolOAuthProviderConfig ceiling: Audience string, Scopes []string (intersection cap)

// internal/tools/drivers/mcp — resolveOAuthBinding now enforces the downstream
// host against the provider allow-list (internal; same signature shape, one new
// refusal branch + a typed error).
```

## Test plan

- **Unit:**
  - Downstream-host allow-list: listed host passes; unlisted host refused;
    host[:port] normalisation (default-port equivalence) pinned; empty
    allow-list on a bindable provider is a boot-validation error.
  - Audience/scope ceiling: audience is the ceiling not the provider name;
    scope intersection drops out-of-ceiling scopes; no ceiling declared →
    documented legacy behaviour preserved (backward-compatible).
  - Hardened token-exchange HTTP client: private/loopback/IP-literal dial
    refused post-DNS; proxy disabled; redirect refused with the typed sentinel;
    caller client shallow-copied (the caller's instance is unmutated).
  - MCP bearer client (WARN-D): a redirect to an unlisted downstream host is
    refused (the exchanged bearer never reaches an off-list host).
  - Broker config (WARN-C): unique names; https/loopback `token_url`; non-empty
    `allowed_downstream_hosts` + `auth_token_env`; the inline `remote` block
    still loads.
  - Audit ordering: a forced emit failure leaves the trust flag unchanged; the
    godoc matches the code.
- **Integration (`test/integration/phase166_credential_sink_test.go`) — binding
  per §17.1 (consumes tools/auth + tools/mcp + protocol + config, all
  shipped):** real drivers end to end against a recording fixture broker + a
  fixture MCP server — a binding to an UNLISTED downstream host is refused at
  attach (the connection never attaches unauthenticated); a redirecting broker
  fixture never receives a re-POSTed `client_secret` (recording assertion: zero
  credential-bearing requests reach the redirect target); identity propagation
  on the attach path; ≥1 failure mode; `-race`.
- **Conformance (§17.8):** the token-exchange leg's request shape derives from
  RFC 8693 (token exchange) — a captured/spec-derived fixture, not a
  hand-authored blob; a wrong-field mutation FAILS. Reuse Phase 142/154 fixtures
  where they already carry the shape.
- **Concurrency / leak:** the hardened client + `resolveOAuthBinding` exercised
  under N≥100 concurrent attaches against shared providers under `-race`; no
  goroutine leak (dial timeouts bound the fetch); no data race.

## Smoke script additions

`scripts/smoke/phase-166.sh` — classification `unit-tests` (this phase adds no
live Protocol surface; the fixtures the security tests need are not run by the
dev boot):

- `go test -race` the `resolveOAuthBinding` downstream-host guard, the
  `tokenexchange` ceiling + hardened-client tests, and the
  `handleSetRawHTMLTrust` audit-ordering test.
- Static: `grep` that `ToolOAuthProviderConfig` carries `AllowedDownstreamHosts`
  and that the token-exchange client construction installs a `CheckRedirect`
  (the regression trip-wires).
- Done-definition: `OK ≥ 2`, `FAIL = 0`.

## Coverage target

- `internal/tools/drivers/mcp` (the binding guard): 85%
- `internal/tools/auth/drivers/tokenexchange` (the ceiling + hardened client): 85%
- `internal/config` (the allow-list + ceiling validation): 90% (existing target)
- `internal/protocol` (the audit-ordering fix): existing target maintained

## Dependencies

- **142** (D-271 — the `tokenexchange` PULL driver being hardened).
- **148** (D-278 — the southbound `oauth_provider` binding whose sink this
  bounds).
- **154** (D-285 — the credential-source seam whose `remote` redirect-refusal is
  the in-repo precedent this copies).
- **28** (the MCP southbound driver + `resolveOAuthBinding`).

## Risks / open questions

- **This changes SHIPPED behaviour.** A deployment that today binds a provider
  to a downstream host it never listed will start failing that binding at
  attach after this phase. That is the correct fail-closed posture, but it IS a
  behaviour change — called out in the CHANGELOG and the migration note, and the
  boot-validation error names the missing `allowed_downstream_hosts` entry so an
  operator's fix is obvious. (An empty allow-list is rejected at boot, so the
  failure surfaces at startup, not at first call.)
- **Backward compatibility of the ceiling.** A provider with NO declared ceiling
  keeps today's audience-from-name behaviour (documented), so existing configs
  load unchanged; the ceiling is opt-in hardening. The allow-list, by contrast,
  is MANDATORY for a bindable provider — that asymmetry is deliberate (an
  unbounded sink is the actual vulnerability; an unbounded audience is a
  lesser lever) and stated.
- **The audit-ordering fix's blast radius.** `handleSetRawHTMLTrust` is the only
  handler with this exact lie, but the compensate-on-emit-failure pattern it
  establishes is the template 168/169 reuse — so the pattern is factored as a
  small reusable helper (apply + emit + revert-on-emit-failure) rather than
  copy-pasted three times.

## Glossary additions

- **Credential sink** — any endpoint a Harbor runtime sends credential material
  to: a token endpoint (where the org's OAuth `client_id`/`client_secret` are
  POSTed for exchange) or a downstream connection host (where an exchanged
  bearer is injected). The v1.14 credential-plane invariant (D-300) is that **no
  admin-writable field may determine a credential sink** — every
  sink-determining value (token endpoint, allowed downstream hosts, audience,
  scope ceiling) is boot-declared, config/file-only. Enforced by the
  boot-declared `allowed_downstream_hosts` allow-list (checked in
  `resolveOAuthBinding`), the boot audience/scope ceiling, and the hardened,
  redirect-refusing token-exchange client. RFC §6.4, §7, D-300.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: N/A — the allow-list + ceiling are
      boot-declared per provider (identity-keying is Phase 167); no
      identity-scoped path changes here beyond what already existed
- [ ] **Concurrent-reuse test passes** — the hardened client +
      `resolveOAuthBinding` under N≥100 concurrent attaches under `-race`
      (D-025)
- [ ] **Integration test exists**
      (`test/integration/phase166_credential_sink_test.go`), wires real drivers
      end-to-end against a recording fixture broker, asserts the unlisted-host
      refusal + the zero-credential-to-redirect assertion, covers ≥1 failure
      mode, runs under `-race`
- [ ] §17.8: the token-exchange fixture derives from RFC 8693 / a captured
      transcript; a wrong-field mutation FAILS
- [ ] Config schema changed → `examples/` updated; backward compatibility
      verified (no-ceiling providers load unchanged); the CHANGELOG migration
      note for the mandatory allow-list is present
- [ ] §18 skill hygiene: `docs/skills/define-the-agent-yaml/SKILL.md`
      (`surface: agent-yaml`) documents the new `allowed_downstream_hosts` +
      ceiling config keys, in the SAME PR
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: N/A — none departed
