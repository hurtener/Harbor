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
- brief 09 §"What to lift from bifrost (concrete)" item 3 (the
  OAuth-discovery option): "`ServerURL` populates `AuthorizeURL` /
  `TokenURL` lazily via `.well-known/oauth-authorization-server`. Reduces
  operator config burden." Harbor already implements that RFC 8414 half
  (`Provider.resolveEndpoints`, `internal/tools/auth/provider.go:858`);
  this phase adds the upstream RFC 9728 step and REUSES the 8414
  fetching/parsing rather than growing a second copy.
- brief 09 §"What to lift from bifrost (concrete)" item 2 (the RFC 7591
  dynamic-registration option): "Implementing it once means operators don't
  have to hand-register a client app per server." The brief's
  recommendation is about the FLOW-side phases; here the
  `registration_endpoint` is REPORTED as data and never invoked
  (`Provider.ensureClient`, `provider.go:924`, stays unused). The
  report-don't-invoke posture is justified NOT by the brief (which is
  positive on registration) but by this phase's custody boundary — invoking
  registration is flow execution, which stays consumer-side by design —
  plus the MCP authorization spec's own framing of discovery metadata as
  advertisement rather than instruction (an explicitly non-brief
  rationale).

## Findings I'm departing from (if any)

None.

## As-built (§4.3 deviation — RFC 8414 hop is a separate SSRF-guarded fetch)

The plan's design prose describes the RFC 8414 hop as "REUSES the existing
`Provider.resolveEndpoints` machinery … no second parser." As built, that is
precise only for the PARSE shape, not the FETCH. The report-only walker's RFC
8414 authorization-server hop is an intentionally SEPARATE fetch path
(`internal/tools/auth/discovery.go` — `fetchHop` at `:394`, the issuer→metadata
URL derivation `authServerMetadataURL` at `:498`), NOT a call into
`Provider.resolveEndpoints` / `fetchDiscovery`. What IS single-homed is the
metadata PARSE struct `discoveredMetadata` (`internal/tools/auth/provider.go:117`),
shared with the interactive-flow resolver.

Why the fetch forks: the report-only hop needs per-hop SSRF guardrails
(cross-origin allowance for the inherently cross-origin AS hop, private-range /
IP-literal refusal via the post-DNS `net.Dialer.Control` backstop, bounded
redirects, size cap, https-only, no credentials) AND typed per-step statuses
(`DiscoveryStepStatus`) that `resolveEndpoints` neither has nor should grow —
`resolveEndpoints` performs an un-guarded fetch and caches into flow-execution
state that report-only discovery must never touch. Composing it directly would
either leak SSRF exposure into the interactive path or contaminate flow state
from a read. The separate walker strengthens the single-homing claim: siblings
85b / 92p reuse THIS guardrailed walker (`auth.Discoverer` → `auth.OAuthRequirement`),
never the ungated flow fetch. Recorded as a permanent §4.3 deviation; D-297's
"As-built (Phase 164, §4.3)" block carries the same correction.

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
     `internal/mcpconsole/mcpconsole.go:141`) TRIGGERS discovery when the
     dial answers the challenge; the discovered requirement is then read via
     the updated connection view (`mcp.servers.get`/`list`) — `probe`'s own
     return stays the existing `MCPProbeRow` (`internal/protocol/mcp.go:
     142-143`), unchanged. Discovery is on-demand, never a background
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
  - **SSRF guardrails on the discovery fetches, specified PER HOP** (the
    `resource_metadata` pointer is attacker-influenceable):
    - *RFC 9728 hop (protected-resource metadata):* same-origin-as-server
      default — a metadata URL on a different origin than the declared MCP
      server URL is refused unless the connection's config explicitly
      allows the named origin.
    - *RFC 8414 hop (authorization-server metadata):* inherently
      CROSS-origin (the AS is normally a different host), so this hop
      always requires the explicit per-connection origin allowance for the
      AS origin(s) — the stated UX consequence: most real discoveries need
      an operator-granted allowance before the AS half of the chain
      populates, and a chain refused at this hop surfaces partially (the
      9728 half + a typed needs-allowance status), never silently empty.
    - *All allowed cross-origin fetches additionally refuse* private-range
      and IP-literal destinations (RFC 1918/4193/loopback/link-local and
      bare-IP hosts) — an allowance names a public origin, never a network
      hole.
    - *Every hop:* bounded redirects, per-fetch timeout, response size cap,
      https-only for non-loopback, and NO credentials of any kind attached.
      Each refusal is a typed, loud error carried in the discovery status —
      never a silent empty result (§13).
- **Consumer, same phase (D-062).** The Console MCP Connections page
  (Phase 108m; a pure `mcp.servers.*` consumer,
  `web/console/src/routes/(console)/mcp-connections/+page.svelte:16-24`)
  renders the discovered requirement on the connection detail: the
  authorization server(s), endpoints, scopes, PKCE posture, and the source
  URL + discovered-at provenance — presented as "discovered requirement
  (unverified — from the connected server)", alongside the existing
  auth-pending state and binding count.
- **Sibling reconciliation (§13 — one mechanism, N consumers).** TWO
  sibling phases plan the SAME 401 → RFC 9728 → RFC 8414 chain with
  flow-execution on top: the parked Phase 92p
  (`docs/plans/phase-92p-mcp-oauth-discovery.md`, reserved D-246 —
  synthesizes an `OAuthConfig` and parks a runtime-side consent flow) and
  the ready Phase 85b (`docs/plans/phase-85b-mcp-http-oauth.md`, master row
  status "Ready now (scope ↑)" — wires `auth.Provider` into the MCP driver
  with RFC 9728 discovery, the `WWW-Authenticate` 401 step-up, and RFC 8707
  resource indicators, running the interactive flow through pause/resume).
  This phase single-homes the DISCOVERY mechanism (challenge capture +
  chain walker) and ships the report-only consumer; when 85b lands — or if
  92p is unparked — they REUSE this phase's discovery output and add their
  flow legs (the Phase 148 precedent: one injection transport, later
  phases reuse it). One discovery implementation, N consumption postures,
  never parallel discovery code. Both sibling plan files carry pointer
  notes recording this (85b's added in the plans PR; 92p's in this
  phase's implementation PR).

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

- [x] The MCP http(s) transport edge captures a `401` +
  `WWW-Authenticate` challenge (with `resource_metadata` when present) and
  records it on the connection's registry state; capture never retries,
  never attaches credentials, never alters the call's error semantics
  (the caller still sees the dial/call failure it sees today).
- [x] The discovery chain fetcher resolves RFC 9728 → `authorization_
  servers[]` → RFC 8414/OIDC metadata, reusing the existing RFC 8414
  fetch/parse (`Provider.resolveEndpoints` composition — no second parser);
  partial chains surface partially with a typed per-step status (metadata
  absent / fetch refused / parse failed), never a silent empty.
- [x] SSRF guardrails pinned by tests, per hop: the 9728 hop's cross-origin
  refusal + explicit-allowance pass; the 8414 hop REQUIRING the allowance
  (and surfacing the typed needs-allowance partial status without it);
  private-range/IP-literal refusal on allowed cross-origin fetches;
  redirect bound enforced; size cap enforced (an oversized body fails
  loud); timeout enforced; https-only for non-loopback; no Authorization /
  cookie headers on any discovery fetch (asserted against a recording
  fixture server).
- [x] `MCPServerView.oauth_requirement` (additive wire type) carries the
  verbatim chain + `discovered_at` + `source` + `source_url`;
  `mcp.servers.list`/`get` project it; `mcp.servers.probe` TRIGGERS
  discovery on a challenge (its own `MCPProbeRow` return is unchanged) and
  the updated view is read via `mcp.servers.get` — one projection home,
  no probe-row wire change.
- [x] §17.8 fixtures derive from the REAL spec artifacts — the CONCRETE
  committed artifacts are the RFCs' own example documents: RFC 9728 §3.2's
  protected-resource-metadata response example and RFC 8414 §3.2's
  authorization-server-metadata response example, committed verbatim as
  `testdata/` fixtures with provenance comments (plus a captured
  `WWW-Authenticate` challenge line per the MCP 2025-06-18 authorization
  spec's example) — never a hand-invented shape; a wrong-field-name
  mutation of the fixture FAILS the test (the right-field/wrong-field
  discriminator).
- [x] Console MCP Connections page renders the discovered requirement
  (endpoints, scopes, PKCE, registration endpoint if advertised, source
  URL, discovered-at) marked as unverified server-supplied data; absent
  cleanly when no discovery has run.
- [x] Hard-boundary negative tests: no token endpoint is ever dialed by the
  runtime during or after discovery (recording fixture asserts zero
  non-metadata fetches); no discovered value lands in any config store.
- [x] Full lockstep in the same PR: `make protocol-ts-gen` +
  `make protocol-docs-gen` (additive wire types on the mcp_servers
  surface). `ProtocolVersion` unbumped.
- [x] `scripts/smoke/phase-164.sh` OK ≥ 2, FAIL = 0.
- [x] `-race`; coverage ≥ 85% on touched Go packages.

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
- `docs/plans/phase-85b-mcp-http-oauth.md` — pointer note recording that
  the 9728/8414 discovery chain is single-homed in this phase's mechanism
  (85b reuses it when it lands) — ADDED IN THE PLANS PR.
- `docs/plans/phase-92p-mcp-oauth-discovery.md` — the same pointer note for
  the parked 92p (added in this phase's implementation PR).
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
  Related, NOT deps: 92p (parked; reserved D-246) and 85b (ready, not yet
  landed) — the flow-executing siblings that reuse this phase's
  single-homed discovery chain; see the sibling-reconciliation goal.

## Risks / open questions

- **The discovery input is adversarial.** The whole feature is parsing
  attacker-influenceable documents fetched from attacker-influenceable
  URLs; the SSRF guardrails + report-don't-follow + no-credential rules are
  the load-bearing mitigations, each with an explicit negative test. The
  adversarial review should attack the guardrail table first.
- **Sibling drift risk (92p + 85b).** If 92p is unparked — or 85b is
  implemented — against a stale memory of their own plans, either could
  grow a second discovery chain; the pointer notes in both plan files +
  D-297's one-mechanism record are the guards.
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

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes
      (the probe/read path identity legs).
- [x] **Reusable-artifact concurrent-reuse:** registry + walker under N≥100
      concurrent probe/read `-race` stress; discovery goroutines joined.
- [x] **Integration test wires real drivers end-to-end (the real MCP http
      transport + spec-derived fixture), asserts identity propagation,
      covers ≥1 failure mode, runs under `-race`** (§17.3 + §17.8).
- [x] Wire changes complete: `make protocol-ts-gen-check` +
      `make protocol-docs-gen-check` green with regenerated artifacts
      committed.
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: N/A — none departed
