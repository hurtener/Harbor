# Phase 164 — MCP OAuth requirement discovery, surfaced as data

## Summary

A second Protocol consumer brokers downstream credentials centrally: it holds
the credential once and the runtime PULLs a fresh token at call time via the
`tokenexchange` credential source (D-271) — the runtime never persists or
refreshes a per-connection credential. To provision that credential the
consumer today hand-declares the provider descriptor (authorization endpoint,
token endpoint, scopes). The MCP authorization spec (2025-06-18) makes
servers ADVERTISE exactly this: an unauthorized call returns `401` +
`WWW-Authenticate: Bearer resource_metadata="…"` pointing at RFC 9728
protected-resource metadata, which names `authorization_servers[]`, whose
RFC 8414/OIDC metadata gives the endpoints and scopes. This phase makes
Harbor's MCP southbound edge DISCOVER that chain and SURFACE it verbatim as
inert Protocol data — an additive field on the MCP-connection view — with
hard boundaries: Harbor NEVER runs the OAuth flow, never holds or refreshes a
token (custody stays consumer-side; D-271 stays PULL), and the discovered
metadata is a report + source URL, never followed and never auto-trusted
config. SSRF guardrails bound the discovery fetches themselves. The D-062
consumer ships in the same phase: the Console MCP Connections page renders
the discovered requirement. Fixtures derive from the real spec artifacts
(§17.8). The parked Phase 92p (runtime-side flow synthesis, reserved D-246)
is the explicitly-reconciled sibling — one discovery mechanism, two
consumers, no parallel implementation (§13).

## RFC anchor

- RFC §6.4
- RFC §5.2
- RFC §6.15
- RFC §7

## Briefs informing this phase

- brief 09
- brief 14

## Brief findings incorporated

- brief 14 §item 9 ("OAuth for HTTP servers"): Harbor's MCP HTTP auth is
  static `Headers` only — "No RFC 9728, no RFC 8707." Verified still true
  for the DETECTION edge: no `401`/`WWW-Authenticate`/`StatusUnauthorized`
  handling exists anywhere in `internal/tools/drivers/mcp/` (grep-verified).
  This phase closes the discovery half of that gap — detection + metadata
  chain — while deliberately NOT closing the flow-execution half (that is
  the parked 92p's territory, and for this consumer the flow is
  consumer-side by design).
- brief 09 §"What bifrost provides": discovery lazily populates
  authorize/token URLs from `.well-known` metadata so operators "don't have
  to hand-register" per server. Harbor already implements the RFC 8414 half
  (`Provider.resolveEndpoints`, `internal/tools/auth/provider.go:858`); this
  phase adds the upstream RFC 9728 step and REUSES the 8414
  fetching/parsing rather than growing a second copy.
- brief 09 §"the dynamic-registration footguns": registration can be
  rejected, demand pre-registered clients, or mismatch redirects — a reason
  this phase REPORTS `registration_endpoint` (RFC 7591) as data and never
  invokes it (`Provider.ensureClient`, `provider.go:924`, stays unused
  here): acting on registration is flow-execution, which is out of scope by
  the custody boundary.

## Findings I'm departing from (if any)

None.

## Goals

- **Detect.** Two triggers, both at the runtime's MCP connection edge (the
  consumer is a pure Protocol client and may have no network path to the
  southbound server — the runtime is the natural discoverer):
  1. an http(s)-transport dial or per-call RPC answered `401` with a
     `WWW-Authenticate` challenge carrying `resource_metadata` (the MCP
     auth-spec step-up) — captured at the driver's HTTP transport edge
     (net-new: nothing captures the challenge today, grep-verified);
  2. an explicit operator/consumer probe: `mcp.servers.probe` (the existing
     verb the Console already drives,
     `internal/mcpconsole/mcpconsole.go:141`) runs discovery when the dial
     answers the challenge — so discovery is on-demand, never a background
     crawler.
- **Discover the metadata chain (report-only).**
  RFC 9728 protected-resource metadata (the challenge's `resource_metadata`
  URL, else `{server}/.well-known/oauth-protected-resource`) →
  `authorization_servers[]` → each server's RFC 8414 / OIDC-discovery
  metadata: `issuer`, `authorization_endpoint`, `token_endpoint`,
  `scopes_supported`, PKCE support (`code_challenge_methods_supported`),
  optional RFC 7591 `registration_endpoint`, and the RFC 8707 `resource`
  identifier. The RFC 8414 fetch/parse REUSES the existing
  `Provider.resolveEndpoints` machinery (read-only composition — one
  implementation, §13); the RFC 9728 fetch is net-new in
  `internal/tools/auth` beside it.
- **Surface as inert data.** An additive field on the MCP-connection view
  the consumer already reads: `MCPServerView`
  (`internal/protocol/types/mcp_servers.go:51-79`, already carrying
  `State` incl. `MCPStateAuthPending` at `:40` and
  `OAuthBindingCount` at `:77-79`) gains
  `oauth_requirement` — the discovered chain VERBATIM plus provenance:
  `{resource_metadata_url, authorization_servers: [{issuer,
  authorization_endpoint, token_endpoint, scopes_supported,
  code_challenge_methods_supported, registration_endpoint?, resource?}],
  discovered_at, source: "challenge"|"probe", source_url}`. Report, don't
  follow: Harbor never dials any discovered endpoint beyond the metadata
  chain itself.
- **HARD boundaries (binding, D-297):**
  - Harbor NEVER runs the authorization-code exchange, never holds or
    refreshes a token, never caches a per-connection credential — custody,
    acquisition, and refresh stay consumer-side; the runtime keeps pulling
    the brokered token via the `tokenexchange` credential source (D-271
    PULL, never PUSH) bound by the declared `oauth_provider` name.
  - Discovered metadata is UNTRUSTED input from the connected server —
    surfaced as a PROPOSAL an operator confirms, never auto-applied to any
    config. Nothing in Harbor consumes the discovered endpoints as config
    in this phase.
  - **SSRF guardrails on the discovery fetches** (the `resource_metadata`
    pointer is attacker-influenceable): same-origin-as-server default
    (a metadata URL on a different origin than the declared MCP server URL
    is refused unless the connection's config explicitly allows the named
    origin), bounded redirects, per-fetch timeout, response size cap,
    https-only for non-loopback, and NO credentials of any kind attached to
    discovery fetches. Each refusal is a typed, loud error carried in the
    discovery status — never a silent empty result (§13).
- **Consumer, same phase (D-062).** The Console MCP Connections page
  (Phase 108m; a pure `mcp.servers.*` consumer,
  `web/console/src/routes/(console)/mcp-connections/+page.svelte:16-24`)
  renders the discovered requirement on the connection detail: the
  authorization server(s), endpoints, scopes, PKCE posture, and the source
  URL + discovered-at provenance — presented as "discovered requirement
  (unverified — from the connected server)", alongside the existing
  auth-pending state and binding count.
- **Sibling reconciliation (§13 — one mechanism, two consumers).** The
  parked Phase 92p (`docs/plans/phase-92p-mcp-oauth-discovery.md`, reserved
  D-246) plans the SAME 401 → RFC 9728 → RFC 8414 chain but then
  synthesizes an `OAuthConfig` and parks a runtime-side consent flow. This
  phase ships the shared DISCOVERY mechanism + the report-only consumer;
  if 92p is ever unparked it REUSES this phase's discovery output and adds
  the synthesis/flow leg — the two phases are one discovery implementation
  with two consumption postures, never parallel discovery code. 92p's plan
  gains a pointer note in the same PR.

## Non-goals

- No OAuth flow execution, no token custody, no refresh, no RFC 7591
  dynamic registration CALLS (the endpoint is reported, never invoked), no
  pause/resume parking — the credential plane stays exactly as D-271
  settled it.
- No auto-application of discovered metadata to `oauth_providers` /
  connection config — the descriptor stays operator-confirmed,
  consumer-side.
- No new canonical event type — the discovered requirement rides the
  connection view read (`mcp.servers.list`/`get` + the synchronous `probe`
  response); an event is additive later if a push signal proves needed.
- No stdio-transport discovery (the challenge is an HTTP-auth construct;
  stdio servers have no 401 edge).
- No background re-discovery loop — discovery runs on challenge or on
  probe, full stop.

## Acceptance criteria

- [ ] The MCP http(s) transport edge captures a `401` +
  `WWW-Authenticate` challenge (with `resource_metadata` when present) and
  records it on the connection's registry state; capture never retries,
  never attaches credentials, never alters the call's error semantics
  (the caller still sees the dial/call failure it sees today).
- [ ] The discovery chain fetcher resolves RFC 9728 → `authorization_
  servers[]` → RFC 8414/OIDC metadata, reusing the existing RFC 8414
  fetch/parse (`Provider.resolveEndpoints` composition — no second parser);
  partial chains surface partially with a typed per-step status (metadata
  absent / fetch refused / parse failed), never a silent empty.
- [ ] SSRF guardrails pinned by tests: cross-origin metadata URL refused by
  default and allowed only via explicit per-connection origin allowance;
  redirect bound enforced; size cap enforced (an oversized body fails
  loud); timeout enforced; https-only for non-loopback; no Authorization /
  cookie headers on any discovery fetch (asserted against a recording
  fixture server).
- [ ] `MCPServerView.oauth_requirement` (additive wire type) carries the
  verbatim chain + `discovered_at` + `source` + `source_url`;
  `mcp.servers.list`/`get` project it; `mcp.servers.probe` triggers
  discovery on a challenge and returns the updated view.
- [ ] §17.8 fixtures derive from the REAL spec artifacts: the conformance
  fixture server replays a captured/spec-derived RFC 9728 document and
  RFC 8414 document (field names from the RFCs / the MCP 2025-06-18
  authorization spec — never a hand-invented shape); a wrong-field-name
  mutation of the fixture FAILS the test (the right-field/wrong-field
  discriminator).
- [ ] Console MCP Connections page renders the discovered requirement
  (endpoints, scopes, PKCE, registration endpoint if advertised, source
  URL, discovered-at) marked as unverified server-supplied data; absent
  cleanly when no discovery has run.
- [ ] Hard-boundary negative tests: no token endpoint is ever dialed by the
  runtime during or after discovery (recording fixture asserts zero
  non-metadata fetches); no discovered value lands in any config store.
- [ ] Full lockstep in the same PR: `make protocol-ts-gen` +
  `make protocol-docs-gen` (additive wire types on the mcp_servers
  surface). `ProtocolVersion` unbumped.
- [ ] `scripts/smoke/phase-164.sh` OK ≥ 2, FAIL = 0.
- [ ] `-race`; coverage ≥ 85% on touched Go packages.

## Files added or changed

- `internal/tools/drivers/mcp/` (the http(s) transport edge) — the 401 +
  `WWW-Authenticate` challenge capture onto the registry's connection
  state.
- `internal/tools/auth/` (new sibling file to `provider.go`) — the RFC 9728
  protected-resource fetch + the chain walker composing the existing
  RFC 8414 `resolveEndpoints` machinery (`provider.go:858`), with the SSRF
  guardrail enforcement; `ensureClient` (`provider.go:924`) deliberately
  untouched/unused.
- The MCP registry (`internal/tools/drivers/mcp/` registry state) — the
  discovered-requirement record + the probe-triggered discovery hook
  (`internal/mcpconsole/mcpconsole.go:141` `Probe` path).
- `internal/protocol/types/mcp_servers.go` — the additive
  `oauth_requirement` view types.
- `internal/protocol/mcp.go` + `internal/protocol/transports/control/
  mcp_handler.go` — projection of the new field on list/get/probe.
- `internal/protocol/singlesource/singlesource.go` + generator typeindex
  files.
- `web/console/src/lib/protocol/` (mcp servers TS mirror) + the
  MCP Connections page detail
  (`web/console/src/routes/(console)/mcp-connections/`) — the discovered-
  requirement card.
- `wire-manifest.gen.json` + `docs/site/protocol/types.md` (regenerated).
- Test fixture: a spec-derived OAuth-challenging MCP HTTP fixture server
  (httptest or a sibling of the existing stdio fixture) replaying captured
  RFC 9728/8414 documents, with a recording layer for the
  zero-non-metadata-fetch and no-credential assertions.
- `test/integration/phase164_mcp_oauth_discovery_test.go` (new).
- `docs/plans/phase-92p-mcp-oauth-discovery.md` — a pointer note recording
  the shared-discovery reconciliation (92p reuses this phase's chain if
  unparked).
- `scripts/smoke/phase-164.sh` (new); `docs/plans/README.md`;
  `docs/decisions.md` (D-297); `docs/glossary.md`.

## Public API surface

- Wire: `MCPServerView.oauth_requirement` (additive) + its nested
  requirement/authorization-server view types; no new methods (list/get/
  probe project it).
- Go: the discovery chain walker in `internal/tools/auth` (internal; the
  one implementation 92p would reuse).
- Console: the MCP Connections discovered-requirement card
  (Console-internal).

## Test plan

- **Unit:** challenge parse table (well-formed / missing `resource_
  metadata` / malformed header); chain walker per-step statuses; SSRF
  guardrail table (cross-origin refused / allowed-origin passes / redirect
  bound / size cap / timeout / no-credential assertion); view projection
  round-trip (additive JSON — old clients unaffected).
- **Integration (`test/integration/phase164_mcp_oauth_discovery_test.go`):**
  the spec-derived OAuth-challenging fixture server behind the real MCP
  http transport — dial → challenge captured → probe → full chain
  discovered and projected on `mcp.servers.get`; the wrong-field fixture
  mutation fails (§17.8 discriminator); the zero-non-metadata-fetch +
  no-credential recording assertions; identity propagation on the
  probe/read path + a cross-tenant read refusal (≥1 failure mode);
  `-race`. An env-gated live leg (`HARBOR_LIVE_*`) against a real
  OAuth-protected MCP server is the wave's live-verification step, not CI.
- **Conformance:** the discovery fixtures ARE the spec-conformance
  artifacts (captured/spec-derived documents committed as fixtures per
  §17.8); no driver-seam suite change.
- **Concurrency / leak:** concurrent probe + list reads against one
  registry under `-race` (N≥100) — no torn requirement records, no
  goroutine leaks from discovery fetches (bounded by timeout; joined on
  close).

## Smoke script additions

- unit-tests class (the discovery edge needs the OAuth-challenging fixture
  server, which the live dev boot does not run): `go test -race` the
  discovery chain + SSRF guardrail packages, plus a static grep that the
  additive wire type is registered in the manifest (`wire-manifest.gen.
  json` carries the requirement view) — the live Console rendering is the
  wave's live-verification step.
- Done-definition: `OK ≥ 2, FAIL = 0`; SKIP until the phase ships.

## Coverage target

- `internal/tools/auth` (the chain walker): 85%
- `internal/tools/drivers/mcp` (the challenge capture): 85%
- `internal/protocol/types` + projection: existing targets maintained
- Console: vitest on the requirement-card state fold.

## Dependencies

- 28 (the MCP southbound driver + its http transports), 30 (tools/auth —
  the OAuth provider home whose RFC 8414 machinery this composes), 108m
  (the MCP Connections page the consumer lands on), 118 (D-223 lockstep).
  Related, NOT a dependency: 92p (parked; reserved D-246) — see the
  sibling-reconciliation goal.

## Risks / open questions

- **The discovery input is adversarial.** The whole feature is parsing
  attacker-influenceable documents fetched from attacker-influenceable
  URLs; the SSRF guardrails + report-don't-follow + no-credential rules are
  the load-bearing mitigations, each with an explicit negative test. The
  adversarial review should attack the guardrail table first.
- **92p drift risk.** If 92p is later unparked against a stale memory of
  its own plan, it could grow a second discovery chain; the pointer note in
  92p's plan + D-297's one-mechanism record are the guards.
- **Challenge capture placement.** The MCP driver's http transports have
  multiple dial/call paths; the capture must live at the shared transport
  edge so streamable-http and SSE both surface it — the implementor
  verifies the single choke point exists (or names the two capture sites
  with one shared parser).
- **View growth.** `MCPServerView` is a hot list row; the requirement
  object rides the DETAIL projection (`get`/`probe`) and appears on the
  list row only as a compact presence marker if row size becomes a concern
  — implementor's call, recorded in the payload godoc (§4.3-recordable).

## Glossary additions

- "OAuth requirement discovery (MCP)" (docs/glossary.md, same PR).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
      (the probe/read path identity legs).
- [ ] **Reusable-artifact concurrent-reuse:** registry + walker under N≥100
      concurrent probe/read `-race` stress; discovery goroutines joined.
- [ ] **Integration test wires real drivers end-to-end (the real MCP http
      transport + spec-derived fixture), asserts identity propagation,
      covers ≥1 failure mode, runs under `-race`** (§17.3 + §17.8).
- [ ] Wire changes complete: `make protocol-ts-gen-check` +
      `make protocol-docs-gen-check` green with regenerated artifacts
      committed.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: N/A — none departed
