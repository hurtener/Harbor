# Phase 148 — MCP southbound per-identity OAuth bearer + `_meta` provenance enrichment

## Summary

Closes the gap between Phase 142's brokered credentials and the MCP wire: an MCP connection can now BIND a declared `tools.oauth_providers[]` entry by name (`oauth_provider` — a non-secret provider NAME on the connection config, the runtime descriptor, and the wire descriptor), and every identity-stamped per-call MCP RPC then fetches `prov.Token(ctx, source)` and injects `Authorization: Bearer <tok>` on THAT request only, via a context-aware RoundTripper — the connect-frozen static header map stays for connect-time auth. Alongside the bearer, `_meta` provenance is enriched: `agent_id` (provenance, NEVER an isolation principal — §6 clarifying note) rides a new ctx seam from the run loop, and operators can declare static `meta_annotations` merged verbatim into `_meta` on every call. A bound provider whose `Token()` fails NEVER falls back to an unauthenticated call (§13); a broker `consent_required` surfaces the existing typed `*auth.ErrAuthRequired` and parks on the ONE pause primitive (§7 rule 4).

## RFC anchor

- RFC §6.4 (tool catalog and transports — the MCP southbound driver; the external-provisioning paragraph D-271 added)
- RFC §3.3 (unified pause/resume — the `consent_required` park path the typed error rides)
- RFC §6.16 (Agent Registry — the registration `agent_id` stamped as provenance)

## Briefs informing this phase

- brief 09
- brief 14
- brief 03

## Brief findings incorporated

- brief 09 §"The four MCP auth types" / §"MCPClientConfig's OAuth fields": "`HttpHeaders` is the seam where the runtime asks 'what `Authorization: Bearer …` should I send on this MCP call?' and the provider either returns a fresh token or surfaces an `MCPUserOAuthRequiredError`. This is the single-call surface where the pause-vs-proceed decision crystallises." — Phase 148 builds exactly that single-call seam: per-call `prov.Token(ctx, source)` → inject-or-typed-error, with the typed `*auth.ErrAuthRequired` parking on the unified primitive. The token is resolved at CALL time from verified ctx identity, never frozen at connect.
- brief 14 §2 row 9: "OAuth for HTTP servers — **Absent (for MCP)** — `auth.Provider` exists but is not wired into the MCP driver at all. MCP HTTP auth = static `Headers` only." — the compliance audit's pinned gap. This phase wires the `OAuthProvider` INTERFACE into the driver as the shared injection seam; the interactive-discovery superset (85b's RFC 9728 / `WWW-Authenticate` step-up, 92l's agent-bound token + heuristic replacement) reuses this seam when those phases land — one injection mechanism, no parallel path.
- brief 03 §"Isolation triple": "Every `ToolDescriptor.Invoke` receives [the triple] and propagates it into provider-specific transports (e.g. A2A `metadata.tenant`, MCP `_meta.tenant`)." — `buildIdentityMeta` (`internal/tools/drivers/mcp/mcp.go:1032`) is the shipped consequence; this phase extends the SAME helper (agent provenance + operator annotations) rather than growing a second `_meta` construction site.
- brief 09 §"Agent identity (Harbor's addition)": the acting-subject (`agent_id`) vs requesting-principal (`user_id`) split — "Provenance captures both." Adopted for the provenance half: `_meta` now carries the acting agent alongside the requesting triple, so a multi-runtime deployment's MCP servers (and the gateways in front of them) can attribute calls per agent.

## Findings I'm departing from (if any)

- brief 09 §"Agent identity" also recommends "isolation predicates can key on either [`agent_id` or `user_id`] depending on the scope of the resource." That recommendation was superseded by the settled §6 clarifying note + D-059: `agent_id` is NOT an isolation principal, and nothing Harbor-side keys storage, event filters, or cache entries by it. This plan stamps `agent_id` as PROVENANCE only, states explicitly that servers MUST NOT treat it as an isolation filter, and keeps the bearer's subject/cache-key user-scoped (D-271). Not a new departure — following the settled decision over the older brief; recorded here because the brief text still reads the other way.

## Goals

- **A second consumer needs per-identity southbound credentials.** Phase 142 shipped the `tokenexchange` driver whose only consumer is the catalog `WrapWithOAuth` pre-check (`internal/tools/catalog/catalog.go:475` — which deliberately DISCARDS the token at :492; injection is per-driver). An external deployment coordinating many runtimes needs the brokered per-user bearer to actually reach a shared MCP server per call. This phase is that consumer: token → wire.
- **Non-secret provider-name binding on every MCP connection surface.** `oauth_provider` (yaml) / `OAuthProvider` (Go) / `oauth_provider` (wire) referencing a declared `tools.oauth_providers[]` name (D-095) lands on: `config.MCPServerConfig` (config.go:1117), `agentcfg.MCPConnectionDescriptor` (agentcfg.go:185), wire `AgentConfigMCPConnectionDescriptor` (agentconfig.go:86) + the add-request's embedded descriptor (:384). A provider NAME is not secret material — the descriptor's ":182 invariant" ("Secret auth material … NEVER part of this descriptor") holds: the name selects a config-declared acquisition strategy; the secret stays env-indirected on the provider entry and the minted token stays in memory. Unknown name → fail loud at config validation / attach, listing registered provider names (§4.4 factory-error convention).
- **Per-call bearer injection, D-025-clean.** When a connection binds a provider, every per-call RPC path that stamps identity `_meta` (tool calls, resource reads, resource subscribe/unsubscribe, prompt gets — all five `buildIdentityMeta` call sites) resolves `prov.Token(ctx, source)` (the provider's TTL cache, single-flighted per `(scope, tenant, user, source)` per D-271, absorbs the per-call cost) and threads the token through the per-call `ctx` into a context-aware RoundTripper that sets `Authorization` on that request only. No mutable transport state — the token rides `req.Context()`; the go-sdk v1.6.1 session is long-lived but the per-call ctx reaches the outbound `*http.Request` (asserted by the integration test, not assumed). The connect-frozen `headerInjectingTransport` (transport_sse.go:47) stays for static headers.
- **The rule is binary: identity-stamped call ⇒ bearer-injected call.** Splitting tool calls (injected) from resource/prompt reads (not) would be a half-auth path — a per-user-authorizing server 401s resource reads exactly as it 401s tool calls. One helper, one rule.
- **Fail loud, one pause path.** Bound provider + `Token()` failure → the RPC fails with the typed error BEFORE any wire request; NEVER a silent unauthenticated call. A `tokenexchange` `consent_required` surfaces the existing `*auth.ErrAuthRequired`, which propagates out of `Invoke` to the same runtime catch the catalog wrapper documents ("the runtime catches the typed sentinel and pauses the run via the unified pause/resume primitive") — zero new pause coordination.
- **`_meta` provenance: `agent_id`.** `buildIdentityMeta` additionally stamps `agent_id` when the new ctx seam carries one. The stamp is provenance for the server's audit/attribution — the plan and godoc state that servers MUST NOT treat it as an isolation filter, and Harbor-side nothing keys by it. The key is `agent_id` (not bare `agent`, unlike the triple's bare keys) deliberately: it matches the Protocol wire vocabulary and D-059's "registration identity" framing, and avoids reading as an agent NAME.
- **Operator-declared `meta_annotations`.** A static, non-secret `map[string]string` on the MCP connection surfaces, merged verbatim into `_meta` on every identity-stamped call — deployments carry their own attribution vocabulary without Harbor encoding foreign keys. Reserved-key collision (`tenant`, `user`, `session`, `agent_id`, `traceparent`, `tracestate` per D-073's `_meta` carrier idiom, plus any `io.modelcontextprotocol/`-prefixed key — the spec-reserved namespace) → rejected at config validation / attach, fail loud.
- **Full wire-change hygiene.** The wire-descriptor change runs the complete D-223 lockstep (hand-mirror into `web/console/src/lib/protocol/agentconfig.ts`, `make protocol-ts-gen` manifest regen, the three-way gate green) AND `make protocol-docs-gen` (D-209) in the same PR.

## Non-goals

- **An agent-scoped brokered bearer.** The `tokenexchange` subject and cache key stay user-scoped (`ScopeUser`, keyed `(scope, tenant, user, source)`) per D-271. Making the broker subject agent-aware is a future decision against D-271/D-278 — the `_meta.agent_id` stamp is provenance, not a credential subject.
- **Push injection** — rejected, not deferred (D-271). No credential ever arrives in-band over the Protocol.
- **Removing or reshaping `WrapWithOAuth`'s pre-check path.** It stays untouched; a `tools.entries[]` HTTP tool keeps its catalog-level pre-check. This phase adds the MCP driver's injection half, which the wrapper's godoc explicitly declares "a per-driver concern."
- **HTTP-driver `oauth_provider` binding.** Phase 149's HTTP manifests keep their static `AuthRef`; a future phase may converge them onto this binding shape.
- **The interactive MCP OAuth superset.** 85b (RFC 9728 discovery, `WWW-Authenticate` step-up) and the 92k–92q agent-bound band (typed replacement of the `looksLikeAuthRequired` heuristic, `add_mcp_connection` OAuth flows) stay their own Pending phases; they REUSE this phase's RoundTripper seam when they land. Nothing here blocks or forks them.
- **`remove_mcp_connection`** — deferred (D-237/D-240 posture: pause covers the disable need).
- **Connect-time (initialize/discovery) per-identity auth.** A user-scoped bearer cannot exist before a per-call identity does; servers that gate `initialize` itself keep static `Headers` (or the future agent-bound path). Documented limitation, see Risks.

## Acceptance criteria

- [ ] `config.MCPServerConfig` gains `OAuthProvider string` (yaml `oauth_provider`) + `MetaAnnotations map[string]string` (yaml `meta_annotations`, NOT secret-tagged — documented as passing to the server verbatim); `internal/config/validate.go::validateTools` rejects: an `oauth_provider` naming no declared `tools.oauth_providers[]` entry (message lists declared names), `oauth_provider` on a stdio connection (fail loud — stdio carries no HTTP request to inject into; the binding is a misconfiguration, not an ignorable hint), a static `Authorization` header (case-insensitive) alongside `oauth_provider` (one auth mode per connection — the D-271 "no dual path" rule read per-connection), and any reserved / spec-prefixed `meta_annotations` key or empty key.
- [ ] `agentcfg.MCPConnectionDescriptor` + wire `AgentConfigMCPConnectionDescriptor` (and thus the `add_mcp_connection` request's embedded descriptor) gain the same two non-secret fields; the attach path (`agentcfg/protocol/addconnection.go` → `AttachRequest` → `config.MCPServerConfig`) threads them; attach-time validation mirrors the config-time rules (unknown provider name → loud `failed` connection state, never a silent unauthenticated attach). The descriptor godoc's non-secret invariant is restated to cover the new fields explicitly.
- [ ] D-223 lockstep complete in the same PR: TS mirror in `web/console/src/lib/protocol/agentconfig.ts`, `make protocol-ts-gen` regenerated manifest committed, `make protocol-ts-gen-check` green; `make protocol-docs-gen` regenerated pages committed (D-209).
- [ ] `mcp.Attach` resolves the provider name against a new `AttachDeps.OAuthProviders map[string]auth.OAuthProvider` (populated by `internal/runtime/assemble` from its existing `Deps.OAuthProviders`, assemble.go:129, and by the devstack twin); the resolved instance lands on `mcp.Config` as an immutable construction-time field. The driver imports ONLY the `internal/tools/auth` interface package — no concrete driver import (§13; grep-asserted in the smoke).
- [ ] Per-call injection: each of the five identity-stamped RPC paths, when the Provider carries a bound `auth.OAuthProvider`, calls `Token(ctx, p.source)` and stashes the bearer on the per-call ctx via an unexported key; a `bearerInjectingTransport` (context-aware, layered over the static-header transport) reads `req.Context()` and sets `Authorization` on the cloned request — set LAST, so a stray static `Authorization` that bypassed validation can never shadow the per-identity token (defence-in-depth; the validation rejection is the primary gate). No Provider/transport field mutates per call (D-025).
- [ ] Fail-closed: a bound provider whose `Token()` errors → the RPC returns the error with NO wire request issued (asserted: the fixture server records zero unauthenticated calls under a refusing broker); a `consent_required`-class refusal propagates the typed `*auth.ErrAuthRequired` unwrapped (`errors.As`-reachable) so the runtime's existing catch parks the run on the unified primitive.
- [ ] `buildIdentityMeta` stamps `agent_id` when the ctx carries agent provenance, and merges `MetaAnnotations` (reserved keys re-checked at merge time as an invariant, "impossible by construction" after validation); the triple keys stay byte-identical (`tenant`/`user`/`session` — a golden test pins the map for the no-agent, no-annotations case so v1.9 behavior is unchanged).
- [ ] The agent-provenance ctx seam ships in `internal/tools` (`WithInvokingAgent(ctx, agentID)` / `InvokingAgentFrom(ctx) (string, bool)`) with godoc pinning the §6 clarifying note (provenance, not an isolation principal; absence is valid — a bare embedder run has no agent). Producers: `cmd/harbor/cmd_dev_runloop.go` stamps its non-empty `agentConfigID` at run start, and the `harbortest/devstack` run loop twins it (§17.6 twin discipline; a twin test asserts both stamp identically).
- [ ] D-025 concurrent-reuse: N≥100 concurrent calls through ONE shared `mcp.Provider` with DISTINCT identity triples under `-race`, against a fixture broker minting identity-derived tokens — the fixture MCP server asserts, per request, that the received `Authorization` bearer matches the `_meta` triple on the SAME request (no token bleed across identities — the isolation-critical assertion), plus no cancellation cross-talk and goroutine baseline restored.
- [ ] Integration test (`test/integration/phase148_mcp_southbound_oauth_test.go`) reusing Phase 142's RFC-8693 httptest broker fixture (`phase142_token_exchange_test.go` — spec-derived per §17.8, asserting `grant_type` / `subject_token_type` / client-auth broker-side) + an httptest-hosted MCP fixture server built on the official go-sdk streamable-HTTP handler (real SDK wire shapes, the D-224 pattern — never a hand blob), asserting: (a) per-call `Authorization` arrives and ROTATES when the identity triple changes; (b) `_meta` carries triple + `agent_id` + annotations; (c) the cold-path token-miss → exchange → inject round-trip; (d) failure mode: broker refusal → the tool call fails loud, the server records NO unauthenticated call; `-race` throughout.
- [ ] `examples/` config gains a documented MCP stanza binding `oauth_provider: <tokenexchange-provider>` + a `meta_annotations` block; §18 sweep: any `docs/skills/` playbook with `surface: mcp` or `tools` that enumerates MCP connection config fields updates in the same PR; `docs/site` include-stubs unchanged unless a recipe is touched.

## Files added or changed

- `internal/config/config.go` (+ `validate.go`) — the two `MCPServerConfig` fields + validation
- `internal/agentcfg/agentcfg.go` — descriptor fields
- `internal/protocol/types/agentconfig.go` — wire descriptor fields
- `internal/tools/agent_provenance.go` (+ test) — the ctx seam
- `internal/tools/drivers/mcp/mcp.go`, `attach.go`, `transport_sse.go` (+ `oauth_test.go`, `concurrent_oauth_test.go`) — binding resolution, per-call token fetch, `bearerInjectingTransport`, `buildIdentityMeta` enrichment
- `internal/runtime/agentcfg/protocol/addconnection.go` — attach-request threading + attach-time validation
- `internal/runtime/assemble/assemble.go` — `AttachDeps.OAuthProviders` population; `cmd/harbor/cmd_dev_runloop.go` + `harbortest/devstack/devstack.go` — agent-provenance producers
- `web/console/src/lib/protocol/agentconfig.ts` + `web/console/src/lib/protocol/wire-manifest.gen.json` (regenerated) — D-223 lockstep
- `docs/site/protocol/types.md` (regenerated, D-209)
- `examples/` — the MCP oauth-binding stanza
- `test/integration/phase148_mcp_southbound_oauth_test.go`
- `scripts/smoke/phase-148.sh`
- `docs/glossary.md`, `docs/decisions.md` (D-278), `docs/plans/README.md` (status flip)

## Public API surface

(Operator-facing surface is YAML + wire; Go surface is internal but consumed by later phases.)

- `config.MCPServerConfig.OAuthProvider` / `.MetaAnnotations` (yaml `oauth_provider` / `meta_annotations`); the same pair on `agentcfg.MCPConnectionDescriptor` and wire `AgentConfigMCPConnectionDescriptor`.
- `tools.WithInvokingAgent(ctx context.Context, agentID string) context.Context` / `tools.InvokingAgentFrom(ctx context.Context) (string, bool)` — the agent-provenance ctx seam (85b / 92l consume it when they land).
- `mcp.AttachDeps.OAuthProviders map[string]auth.OAuthProvider` — the resolution seam embedders populate programmatically.

## Test plan

- **Unit:** config validation table (unknown provider name lists declared names; stdio+binding rejected; `Authorization`-header conflict rejected case-insensitively; reserved / spec-prefixed / empty annotation keys rejected); `buildIdentityMeta` golden (unchanged triple bytes; `agent_id` + annotations merge; merge-time reserved-key invariant); `bearerInjectingTransport` (ctx token → header, no-token → no header, static-header layering order); agent-provenance ctx round-trip + absence; attach resolution fail-loud.
- **Integration:** the phase-148 test above (142 broker fixture + go-sdk streamable-HTTP fixture server; rotation, provenance, cold path, broker-refusal fail-loud with zero unauthenticated calls) + a park leg: `consent_required` → typed `*auth.ErrAuthRequired` reaches the runtime catch. Real drivers on every seam, identity propagated, ≥1 failure mode, `-race` (§17).
- **Conformance:** N/A — no new driver seam; the binding consumes the existing D-095 registry and `auth.OAuthProvider` interface.
- **Concurrency / leak:** the N≥100 mixed-triple shared-Provider test above (token-bleed assertion broker+server-side), plus goroutine-baseline after provider `Close` mid-flight-cancel.

## Smoke script additions

- `scripts/smoke/phase-148.sh` (`PREFLIGHT_REQUIRES: unit-tests`): `go test -race` legs for `internal/tools/drivers/mcp` (the oauth + provenance tests), `internal/config` (the validation table), `internal/tools` (the provenance seam), and the phase-148 integration test; a config-validation leg driving the validator against an unknown-provider fixture and asserting the error names the declared providers; static greps asserting (a) no concrete `tools/auth/drivers/*` import inside the MCP driver and (b) the wire manifest was regenerated (gate reuse). Skeleton parks with `skip` until the surface lands.

## Coverage target

- `internal/tools/drivers/mcp`: ≥ 80% on touched files
- `internal/config`, `internal/agentcfg`, `internal/tools` (touched lines): no regression below current package coverage

## Dependencies

- 142 (the `tokenexchange` driver + D-271 — the credential this phase carries to the wire)
- 92f (`add_mcp_connection` — the runtime descriptor + attach surface extended)
- 28 + the 85-band MCP driver phases (the southbound MCP driver + its compliance state this phase mutates)
- 30 + the D-095 registry via 64a (the `auth.OAuthProvider` interface + named-provider config)
- 50 (unified pause/resume — the `consent_required` park path)
- 118 (D-223 — the TS lockstep gate the wire change must satisfy)

## Risks / open questions

- **Per-call ctx → outbound request is a go-sdk behavior, not a Harbor invariant.** v1.6.1 threads the per-call ctx into the transport's `*http.Request`; a future SDK bump that pools/detaches requests would silently strip the bearer. Mitigation: the integration test asserts header arrival per call (a strip fails CI, not production), and the RoundTripper treats bearer-absence on a bound connection's per-call POSTs as un-assertable at that layer — the driver-side fail-closed check (Token error → no request) is the load-bearing gate.
- **Connect-time auth is out of scope.** `initialize` / discovery run before any per-call identity exists; a server that 401s the handshake needs static `Headers` (or 85b/92l later). Named in Non-goals; the examples stanza documents the split so operators aren't surprised.
- **Keepalive pings / SSE stream GETs carry no per-call token** (no ctx identity) — only static headers. A server that authorizes per-request-uniformly must accept the connect-time posture on those; documented in the driver godoc.
- **Composition with 85b / 92l.** Both Pending phases inject `Authorization` too. This phase's RoundTripper + `Config` binding is deliberately the SHARED seam: 92l resolves an agent-bound token through the same injection path; 85b adds discovery in front of it. Review gate for those phases: extend the seam, never add a second injection transport (§13 two-parallel-implementations). D-278 records the agreement.
- **`_meta` size + trust.** Annotations pass verbatim and uncapped beyond validation; a pathological operator map bloats every RPC. Accepted: operator-declared config, not caller input; validation rejects reserved keys, and the audit posture is unchanged (annotations are non-secret by contract, stated in godoc + example).

## Glossary additions

- **Southbound OAuth binding** — the non-secret `oauth_provider` name on an MCP connection that selects a declared OAuth provider for per-call bearer injection.
- **Agent provenance** — the ctx-carried registration `agent_id` stamped into southbound `_meta` for attribution; never an isolation principal (§6).
- **Meta annotations** — operator-declared static key/values merged verbatim into an MCP connection's per-call `_meta`.

(All added to `docs/glossary.md` in the same PR as this plan.)

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes (the mixed-triple token-bleed test IS the isolation test for this surface)
- [ ] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. (The bound Provider is the reusable artifact — the mixed-triple test is mandatory.)
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md §17. (It consumes 142 + 92f + 28 + 30 + 50 — the integration test is mandatory.)
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (the brief-09 isolation-predicate note — already settled by §6/D-059, restated in D-278)
