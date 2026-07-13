# Phase 166 — Live MCP OAuth discovery-allowance write

> Opens the v1.14 wave (`docs/plans/wave-v114-coordination.md`). Decision:
> **D-300**. Raised by an external white-label implementor (HA-15): "a Protocol
> write (or a live allowance surface) for the MCP OAuth-discovery ALLOWED
> ORIGINS and the oauth-provider binding." This phase is the allowance half;
> Phase 167 is the provider half.

## Summary

Phase 164 (D-297) shipped MCP OAuth-requirement discovery, but its RFC 8414
authorization-server hop is inherently cross-origin and therefore requires an
explicit per-connection origin allowance — which today exists ONLY as the
restart-required yaml field `mcp.servers[].oauth_discovery_allowed_origins`
(`internal/config/config.go:1308`). A Protocol-driven consumer that adds MCP
connections at runtime has no way to grant it, and — a shipped bug this phase
fixes in-band per CLAUDE.md §17.6 — the runtime add path DROPS the field
entirely, so every runtime-added connection's discovery is permanently stuck at
`needs_allowance`. This phase makes the allowance a first-class, revisioned,
diffable, rollback-able field on the agent-config connection descriptor; adds
ONE narrow admin-gated Protocol verb (`agent_config.set_mcp_discovery_origins`)
that writes it on the revision spine AND applies it to the LIVE connection; and
ships the D-062 Console consumer that closes the see-it-here / fix-it-there gap
between the MCP Connections page and the Agent Config page.

## RFC anchor

- RFC §6.4 — the tool catalog and the MCP southbound edge (the discovery
  walker's allowance input and the registry that snapshots it).
- RFC §6.16 — the agent-config control plane (the revision spine the write
  rides: versioned, diffable, rollback-able).
- RFC §5.2 — what the Protocol exposes (the new admin write verb + the
  additive descriptor field).
- RFC §6.15 — the governance / audit posture (the admin-scope audit event on
  the write; fail-closed on an un-auditable grant).
- RFC §7 — the Console lens (the two-page consumer).

## Briefs informing this phase

- brief 09
- brief 14
- brief 11

## Brief findings incorporated

- **brief 09 §"What bifrost provides" (the OAuth-discovery option):** discovery
  "reduces operator config burden" by populating endpoints lazily from
  well-known metadata. Phase 164 built the walker, but a config-only,
  restart-required allowance re-imposes exactly the operator burden the brief
  says discovery removes — for a Protocol-driven consumer it is not merely a
  burden, it is a wall (no yaml, no restart). This phase makes the allowance
  reachable from the same control plane that adds the connection.
- **brief 09 §"the dynamic-registration footguns":** every failure branch fails
  loud — no silent fallback to an unauthenticated dial. Applied here to the
  WRITE path: a malformed origin, an unknown connection, and a boot-declared
  connection each fail loud with a distinct typed error; a grant that cannot be
  audited is refused (the call fails closed), never silently applied.
- **brief 14 §item 9 ("OAuth for HTTP servers"):** the canonical MCP auth
  sequence is discovery-driven. This phase does not add a second discovery
  chain — the walker (`internal/tools/auth/discovery.go`) and its per-hop SSRF
  guardrails are untouched. It only feeds the walker's EXISTING
  `DiscoveryInput.AllowedOrigins` input (`discovery.go:167`) from a live,
  revisioned source instead of a boot-frozen one.
- **brief 11 §"MCP Connections view":** the Console's MCP Connections page is
  the operator's connection lens. Applied here with a hard boundary: the page
  is where the operator SEES `needs_allowance`, but it is NOT where the write
  lives — the write is single-homed on the Agent Config connection surface
  (where revisions/diff/rollback are already rendered), and the MCP Connections
  page deep-links to it. One write affordance, not two (§13 — no parallel
  implementations of the same conceptual feature).

## Findings I'm departing from (if any)

None.

## Goals

- **Make the allowance revisionable (gap 1).**
  `agentcfg.MCPConnectionDescriptor` (`internal/agentcfg/agentcfg.go:220-243`)
  gains `OAuthDiscoveryAllowedOrigins []string` — a NON-SECRET field, so it
  belongs in the persisted descriptor exactly as `OAuthProvider` (a name, not a
  secret) does. It therefore inherits the whole spine for free: `SetRevision`,
  `Diff`, `Rollback`, `ListRevisions`.
- **Fix the shipped field-drop bug in-band (gap 2 — §17.6).**
  `MCPConnectionAttacher.Attach` (`internal/runtime/serve/mcp_attacher.go:79-87`)
  builds a `config.MCPServerConfig` and OMITS `OAuthDiscoveryAllowedOrigins`.
  The downstream chain is otherwise intact and verified —
  `mcpdrv.Attach` copies it (`internal/tools/drivers/mcp/attach.go:189`) →
  `Registry.Register` snapshots it (`registry.go:363`) →
  `Registry.OAuthDiscoveryTarget` returns it (`registry.go:647-660`) →
  `internal/mcpconsole/mcpconsole.go` feeds it to `Discoverer.Discover` — so the
  single dropped field makes Phase 164's discovery INERT for every
  runtime-added connection: its AS hop always reports `needs_allowance`. The
  fix is the `AttachRequest` + `Attach` carry. This is a previously-shipped
  phase's bug fixed in THIS PR per §17.6, with a regression test that fails
  without the carry.
- **Add ONE narrow admin write verb.** `agent_config.set_mcp_discovery_origins`
  — request `{agent_id, name, allowed_origins[]}`, FULL REPLACE (not add/remove
  deltas: a replace is the only semantic that diffs and rolls back cleanly).
  It writes a new revision under `lockAgent` carrying every sibling section
  forward (the D-283 rebuild-completeness guard), AND applies the new set to
  the live MCP registry so the very next discovery uses it.
- **Make the write LIVE, and make REVOKE live too (gap 3 + hard constraint 3).**
  The allowance is a pure per-call policy input to the walker (read fresh on
  every `Discover` via `OAuthDiscoveryTarget`; normalised per-call at
  `discovery.go:337`) — nothing about the live transport depends on it. So the
  registry gains a mutator, `Registry.SetOAuthDiscoveryOrigins`, modelled on
  `SetRawHTMLTrust` (`registry.go:834-847`): identity-mandatory, mutex-guarded,
  returns the prior value so a caller can detect a no-op. **Revoke's live
  effect is the load-bearing half:** dropping an origin must not merely stop the
  NEXT discovery — it must also invalidate the requirement data a
  now-revoked allowance already produced. So the same call PRUNES the recorded
  `oauth_requirement`'s authorization-server entries that were fetched from a
  revoked origin and re-stamps their `needs_allowance` per-hop status. That is
  the honest "detach" analogue for an allowance (the D-287 lesson: v1.11 shipped
  a detach-only reconcile and ate the asymmetry — this phase ships the write and
  its revoke with symmetric live effect, in the same PR).
- **Reuse the shipped origin validator (hard constraint 2).**
  `validateDiscoveryOrigin` (`internal/config/validate.go:2276-2301` — https-only,
  host required, no path/query/fragment, IP-literal rejected) is EXPORTED as
  `config.ValidateDiscoveryOrigin` and the existing unexported validator becomes
  a one-line caller. ONE implementation, two call sites (boot validation + the
  Protocol write). No second origin parser.
- **Prove allowance ≠ SSRF bypass (hard constraint 2).** A runtime-granted
  origin is still dial-guarded: the walker's `net.Dialer.Control` hook
  (`discovery.go:260-281`) runs PER-DIAL, POST-DNS-RESOLUTION, so an origin whose
  hostname resolves to a private / loopback address is STILL refused. Pinned by a
  named test — a runtime write cannot widen the SSRF surface, only the
  origin allow-list the walker consults before it dials.
- **Authorization is server-derived and `admin`-gated (hard constraint 4).**
  The new route is admin-gated FOR FREE: `AgentConfigHandler`'s scope gate
  (`internal/protocol/transports/stream/agentconfig_handler.go:133-155`) requires
  `auth.HasScope(ctx, auth.ScopeAdmin)` on its `default:` arm, and the new route
  is simply NOT added to the session-safe (`:224-230`) or user (`:238-244`)
  exception maps. Identity comes from `resolveIdentity(r)` / the service's
  `identityFromScope` — NEVER from the request body (D-219). No new scope is
  minted; the scope set stays CLOSED (D-284).
- **Audit the grant, fail closed on an un-auditable one.** The write emits an
  admin-scope audit event carrying the agent, connection name, and the
  granted/revoked origin sets (all non-secret). A failed emit fails the CALL
  closed, exactly as `handleSetRawHTMLTrust` does (`internal/protocol/mcp.go:
  843-872` — the audit-emit-and-fail-closed posture lives at the SURFACE
  handler, not in the registry setter).
- **Console consumer, same wave (D-062 + §13).** Resolved deliberately, see
  "Console consistency" below.
- **Give `remove_mcp_connection` its first Svelte caller.** The verb shipped in
  Phase 156 (D-287) and its TS client method exists
  (`web/console/src/lib/protocol/client.ts:1190`) with NO Svelte caller —
  grep-verified. The Agent Config connections card this phase turns into a real
  editor is its honest home: a remove affordance beside the grant/revoke
  affordance, on the same revisioned surface. Included because it belongs
  there, not because it is cheap.

## Non-goals

- **No change to the discovery walker or its SSRF guardrails.**
  `internal/tools/auth/discovery.go` is untouched. This phase feeds its existing
  `AllowedOrigins` input; it does not widen, re-shape, or bypass any hop guard.
- **No general `update_mcp_connection` patch verb.** A patch verb inviting
  `url` / `command` / `transport` / `oauth_provider` edits would be a silent
  half-write: those fields are consumed at ATTACH time and a live connection
  holds them, so patching them without a re-attach changes the revision and not
  the runtime. Changing them stays remove + re-add. The allowance is the ONE
  field that is re-read per discovery call, which is exactly why it gets a
  narrow, purpose-built verb.
- **No OAuth-provider descriptor install, and no connection→provider binding
  work** — that is Phase 167.
- **No boot-declared (yaml) connection editing.** The verb governs revisioned
  state only. A boot-declared connection's allowance is edited in `harbor.yaml`
  and applied by a restart; the verb fails loud with the distinct typed error
  Phase 156 already established for boot-declared names. The Console says so in
  copy rather than
  offering a button that cannot work.
- **No OAuth flow execution, no token custody.** D-297's hard boundaries are
  unchanged: Harbor still never runs the flow, never holds a token, never dials
  a discovered endpoint. This phase only writes the allow-list the walker
  consults.
- **No re-discovery trigger on grant.** Granting an origin does not
  auto-re-walk the chain — the operator re-probes (`mcp.servers.probe`, which
  already triggers discovery). A background re-crawler stays a decided-NO
  (D-297).

## Acceptance criteria

- [ ] `agentcfg.MCPConnectionDescriptor` carries
      `OAuthDiscoveryAllowedOrigins []string` (non-secret, `omitempty`); the
      field round-trips through `SetRevision` / `Active` / `Diff` /
      `Rollback` on all three StateStore drivers, and the
      `rebuild_completeness_test.go` reflection walk
      (`internal/runtime/agentcfg/protocol/`) passes — every section setter
      carries the new field forward (D-283).
- [ ] **In-band shipped-bug fix (§17.6):** `AttachRequest` carries the
      allowance and `MCPConnectionAttacher.Attach` sets
      `config.MCPServerConfig.OAuthDiscoveryAllowedOrigins`. A regression test
      adds a connection with an allowance over the Protocol, then asserts
      `Registry.OAuthDiscoveryTarget` returns it — the test FAILS against the
      pre-fix attacher (the discriminator).
- [ ] `agent_config.set_mcp_discovery_origins` is registered in EVERY canonical
      home: `methods.go` const + `canonicalMethods` + `canonicalAgentConfigMethods`
      + `canonicalAgentConfigAdminMethods`, `singlesource.go`,
      `conformance.go`, `cmd/harbor-gen-protocol-docs/methods.go`; and
      `make protocol-ts-gen` + `make protocol-docs-gen` are re-run with the
      regenerated `wire-manifest.gen.json` + `docs/site/protocol/*.md`
      committed (D-223 / D-209 — all three lockstep gates green).
- [ ] The verb is **admin-gated** and the gate is test-pinned: a caller with a
      valid identity but NO `admin` scope is rejected with `CodeScopeMismatch`
      BEFORE any dispatch; a caller with the `agent_config:user` scope is ALSO
      rejected (the user tier does not admit an admin route). Authority is read
      from the verified ctx, never the request body (D-219).
      Test: `TestSetMCPDiscoveryOrigins_RejectsNonAdminScope`.
- [ ] Every origin on the write path is validated by the SHARED
      `config.ValidateDiscoveryOrigin` (exported from
      `internal/config/validate.go`; the boot validator becomes its caller —
      one implementation). A non-https origin, an origin with a path / query /
      fragment, an empty origin, and an IP-literal host each fail loud with a
      typed `CodeInvalidRequest` naming the offending entry.
      Test: `TestSetMCPDiscoveryOrigins_RejectsMalformedOrigin`.
- [ ] **Allowance ≠ SSRF bypass (hard constraint 2).** A runtime-granted origin
      whose hostname resolves to a private / loopback address is STILL refused
      at dial time by the walker's post-DNS `net.Dialer.Control` guard, and the
      chain surfaces the typed refusal — never a fetch.
      Test: `TestDiscovery_RuntimeGrantedOrigin_StillRefusesPrivateDial`
      (fails-without / passes-with the guard).
- [ ] **The write is LIVE.** After a successful call, `OAuthDiscoveryTarget`
      for that connection returns the new set WITHOUT a restart or a re-attach,
      and the next `mcp.servers.probe`-triggered discovery walks the previously
      refused AS hop.
- [ ] **REVOKE is LIVE and symmetric (hard constraint 3).** Removing an origin
      from the set (a) makes the next discovery refuse that hop with the typed
      `needs_allowance` status, AND (b) PRUNES the already-recorded
      `oauth_requirement`'s authorization-server entries whose `source_url`
      origin was revoked, re-stamping their per-hop status. The connection view
      never keeps rendering data obtained under a withdrawn allowance.
      Test: `TestSetMCPDiscoveryOrigins_RevokePrunesRecordedRequirement`.
- [ ] The registry mutator `Registry.SetOAuthDiscoveryOrigins` is
      identity-mandatory, mutex-guarded ("internally synchronised" per D-025),
      returns the prior set, and is safe under N≥100 concurrent
      writes+`OAuthDiscoveryTarget` reads under `-race` with no torn slice.
- [ ] The write emits ONE admin-scope audit event (non-secret payload: agent id,
      connection name, granted set, revoked set). **A failed audit emit fails
      the CALL closed** — an un-auditable grant is refused, never silently
      applied (the `handleSetRawHTMLTrust` posture, `internal/protocol/mcp.go:
      860-868`). Test: `TestSetMCPDiscoveryOrigins_FailsClosedOnAuditEmitFailure`.
- [ ] Distinct typed errors, each fail-loud: unknown connection name; a
      **boot-declared** (yaml) connection name (the verb governs revisioned state
      only — reuse Phase 156's typed error); a connection on the `stdio`
      transport (no HTTP 401 edge exists — an allowance is meaningless and is
      refused rather than silently stored).
- [ ] **Console consumer (D-062), both pages, one write affordance:** the Agent
      Config connections card gains the grant/revoke editor (+ the
      `remove_mcp_connection` affordance), and the MCP Connections detail rail's
      `needs_allowance` status renders the refused origin verbatim with a
      deep-link to the Agent Config editor. A boot-declared connection renders
      the yaml-edit copy and NO link. Console vitest covers both branches.
- [ ] `scripts/smoke/phase-166.sh` OK ≥ 3, FAIL = 0.
- [ ] `-race` green; coverage ≥ the stated target on every touched Go package.

## Files added or changed

- `internal/agentcfg/agentcfg.go` — `MCPConnectionDescriptor.OAuthDiscoveryAllowedOrigins`
  (gap 1).
- `internal/config/validate.go` — export `ValidateDiscoveryOrigin`; the existing
  `validateDiscoveryOrigin` (`:2276`) becomes its caller (one implementation).
- `internal/runtime/serve/mcp_attacher.go` — the `AttachRequest` → `config.MCPServerConfig`
  allowance carry (**the in-band shipped-bug fix, gap 2**).
- `internal/runtime/agentcfg/protocol/addconnection.go` — `AttachRequest` gains the
  field; `recordConnectionRevision` persists it.
- `internal/runtime/agentcfg/protocol/setdiscoveryorigins.go` (new) — the service
  method: validate → `lockAgent` → revision write (siblings carried forward) →
  live registry apply → revoke-prune → audit emit (fail-closed).
- `internal/tools/drivers/mcp/registry.go` — `SetOAuthDiscoveryOrigins` mutator
  (gap 3) + the revoked-origin requirement prune.
- `internal/protocol/methods/methods.go` — the method const +
  `canonicalMethods` + `canonicalAgentConfigMethods` +
  `canonicalAgentConfigAdminMethods`.
- `internal/protocol/types/agentconfig.go` — the request/response wire types +
  `AgentConfigMCPConnectionDescriptor.oauth_discovery_allowed_origins`
  (additive).
- `internal/protocol/transports/stream/agentconfig_handler.go` — the route
  (admin-gated by inheriting the `default:` arm — NOT added to either exception
  map), reusing `decode` / `assertIdentity` / `writeServiceError`.
- `internal/protocol/singlesource/singlesource.go`,
  `internal/protocol/conformance/conformance.go`,
  `cmd/harbor-gen-protocol-docs/methods.go` + the `typeindex.go` files — the
  three lockstep registration homes (D-223).
- `web/console/src/lib/protocol/` (the agent-config wire mirror + client method),
  `web/console/src/lib/agentconfig/state.svelte.ts` + the connections card
  (grant/revoke editor + the remove affordance),
  `web/console/src/lib/components/mcp-connections/McpDetailRail.svelte` (the
  `needs_allowance` deep-link).
- `wire-manifest.gen.json` + `docs/site/protocol/{methods,types}.md`
  (regenerated, never hand-edited).
- `test/integration/phase166_discovery_allowance_test.go` (new).
- `scripts/smoke/phase-166.sh` (new); `docs/plans/README.md`;
  `docs/decisions.md` (D-300); `docs/glossary.md`.

## Public API surface

```go
// Wire (additive):
//   agent_config.set_mcp_discovery_origins
//     req  {agent_id, name, allowed_origins[]}   // FULL REPLACE
//     resp {revision, granted[], revoked[], applied_live bool}
//   AgentConfigMCPConnectionDescriptor.oauth_discovery_allowed_origins []string

// internal/config — the ONE origin validator, now shared with the write path.
func ValidateDiscoveryOrigin(origin string) error

// internal/tools/drivers/mcp — the live allowance mutator. Identity is
// mandatory; internally synchronised (D-025). Returns the prior set so a
// caller can detect a no-op and compute the revoked delta.
func (r *Registry) SetOAuthDiscoveryOrigins(ctx context.Context, name string, origins []string) (prev []string, err error)
```

## Test plan

- **Unit:**
  - Descriptor round-trip through `SetRevision` / `Active` / `Diff` /
    `Rollback`; the D-283 rebuild-completeness reflection walk.
  - Origin-validation table on the write path (non-https / path / query /
    fragment / empty / IP-literal / valid) — asserting the SHARED validator is
    the one that rejects (a second parser would diverge).
  - Scope gate: no-scope → `CodeScopeMismatch`; `agent_config:user` scope →
    `CodeScopeMismatch`; `admin` → dispatch. Authority never read from the body.
  - Typed-error table: unknown name / boot-declared name / stdio transport.
  - Audit fail-closed: a forced redactor/emit failure fails the CALL.
  - Revoke-prune: a recorded `oauth_requirement` whose AS entry came from a
    now-revoked origin is pruned and re-stamped `needs_allowance`.
- **Integration (`test/integration/phase166_discovery_allowance_test.go`) —
  binding per §17.1 (this phase consumes agentcfg + tools/mcp + tools/auth +
  protocol, all shipped subsystems):** real drivers end-to-end against the
  spec-derived OAuth-challenging MCP fixture server Phase 164 committed —
  (1) `add_mcp_connection` with an allowance → assert the registry SEES it (the
  gap-2 regression discriminator: FAILS without the attacher fix);
  (2) probe → the AS hop is walked (previously `needs_allowance`);
  (3) `set_mcp_discovery_origins` revoking the origin → next probe refuses the
  hop AND the recorded requirement's AS entry is pruned — **live effect, no
  restart** (hard constraint 3);
  (4) **identity propagation** through the triple on the write + read path, and
  a cross-tenant write refusal (≥1 failure mode);
  (5) `-race`.
- **Conformance:** §17.8 — the fixture is Phase 164's committed RFC 9728 §3.2 +
  RFC 8414 §3.2 example documents (the real spec artifacts, with provenance
  comments), NOT a hand-authored blob. Any NEW fixture this phase adds for the
  allowance behaviour derives from the same spec artifacts or a captured real
  transcript; a wrong-field mutation must FAIL the test (the
  right-field/wrong-field discriminator).
- **Concurrency / leak:** N≥100 concurrent `SetOAuthDiscoveryOrigins` writes
  interleaved with `OAuthDiscoveryTarget` reads and `Discover` walks against ONE
  shared `Registry` under `-race` — no torn slice, no cross-connection bleed, no
  goroutine leak after teardown (D-025; the registry is a compiled artifact).
- **Security (named, binding):**
  - `TestDiscovery_RuntimeGrantedOrigin_StillRefusesPrivateDial` — a granted
    origin whose DNS resolves to loopback/private is refused at dial by the
    post-DNS `Dialer.Control` guard. Allowance ≠ SSRF bypass.
  - `TestSetMCPDiscoveryOrigins_RejectsNonAdminScope`.
  - `TestSetMCPDiscoveryOrigins_FailsClosedOnAuditEmitFailure`.
  - `TestSetMCPDiscoveryOrigins_RevokePrunesRecordedRequirement`.

## Console consistency

Binding per CLAUDE.md §4.5 item 12 — this section cites
`docs/design/console/CONVENTIONS.md` (D-121) and
`docs/design/console/PAGE-POLISH-PROCEDURE.md`, and the page work is built
against them: the ONE `(console)` route group with unprefixed URLs (§1), the
shared app shell (§2), the `web/console/src/lib/components/ui/` inventory (no
hand-rolled primitives), the four-state `<PageState>` async contract, the
unified `HarborClient` + `connection.ts` (no hand-rolled `fetch` — §4.5 item 5),
and `tokens.css` only (no raw color / spacing literals — stylelint enforces).

**The page split — resolved deliberately, not left to the implementor.**
The operator SEES `needs_allowance` on `/mcp-connections`
(`McpDetailRail.svelte:296-344`, the status list at `:336-342`), but every
connection WRITE lives on `/agent-config`
(`AddConnectionCard.svelte` + `state.svelte.ts:901-942`). Two defensible homes,
so the choice is recorded here and in D-300:

1. **The write is single-homed on `/agent-config`.** The allowance is a field of
   the revisioned connection descriptor. A revisioned write with no
   diff/rollback affordance beside it is dishonest, and `/agent-config` is the
   page that already renders revisions, diff, and rollback (Phase 92h/92i). So
   the Agent Config connections card becomes a real EDITOR: per-connection
   grant/revoke of discovery origins, plus the `remove_mcp_connection`
   affordance (Phase 156's verb, currently caller-less). One write affordance
   for one verb (§13 — never two parallel implementations of one feature).
2. **`/mcp-connections` gets a DEEP-LINK, never a second write form.** The
   detail rail's existing `needs_allowance` status renders the refused ORIGIN
   verbatim and links to `/agent-config?connection=<name>&grant_origin=<origin>`
   (unprefixed inter-page link per CONVENTIONS §1). The Agent Config page reads
   the query params and pre-fills the grant field. This closes the
   see-it-here / fix-it-there gap without forking the write.
3. **Boot-declared connections render honesty copy, not a broken button.** The
   verb governs revisioned state only; a yaml-declared connection shows "edit
   `oauth_discovery_allowed_origins` in harbor.yaml and restart" and NO link
   (mirrors Phase 156's boot-declared typed refusal). Console vitest pins both
   branches.

## Smoke script additions

`scripts/smoke/phase-166.sh` — classification `live-server` (the write verb is
served by the booted dev stack), with a `unit-tests` companion class for the
SSRF/dial-guard leg (which needs the fixture server the dev boot does not run):

- `agent_config.set_mcp_discovery_origins` is present in the booted server's
  method surface (a 404/405/501 → SKIP keeps the script green on pre-166
  builds).
- A grant against an UNKNOWN connection name returns the typed loud error
  (never a silent 200).
- A call WITHOUT the admin scope is rejected with `CodeScopeMismatch` (the
  scope gate, exercised over the wire).
- A malformed origin (`http://…`, an origin with a path, an IP literal) is
  rejected with `CodeInvalidRequest`.
- Static: the new method appears in `wire-manifest.gen.json` (the D-223 lockstep
  trip-wire) and in the regenerated `docs/site/protocol/methods.md`.
- `go test -race` the registry mutator + discovery dial-guard packages.
- Done-definition: `OK ≥ 3`, `FAIL = 0`.

## Coverage target

- `internal/runtime/agentcfg/protocol` (the new service method): 85%
- `internal/tools/drivers/mcp` (the registry mutator + prune): 85%
- `internal/agentcfg` (the descriptor field + diff arm): 85%
- `internal/config` (the exported validator): existing target maintained (90%)
- `internal/protocol/types` + the stream handler: existing targets maintained
- Console: vitest on the grant/revoke editor state fold + the deep-link /
  boot-declared branches.

## Dependencies

- **164** (D-297 — the discovery walker + the registry's challenge/allowance
  state + the `needs_allowance` status this phase makes actionable).
- **92f** (the shipped `add_mcp_connection` verb + the `ConnectionAttacher`
  seam this phase fixes).
- **156** (D-287 — the connections diff arm, the boot-declared typed refusal,
  and the `remove_mcp_connection` verb this phase gives its first caller).
- **152** (D-283 — the rebuild-completeness guard the new field must satisfy).
- **92h / 92i** (the Console Agent Config panel + revision UX the editor lands
  in), **108m** (the MCP Connections page the deep-link leaves from).
- **118** (D-223 — the TS/docs lockstep gates).

## Risks / open questions

- **The allowance is a security control being made runtime-writable.** This is
  the central risk and the reason hard constraints 2 and 4 are named tests, not
  prose. Mitigations, each with a test: the write is admin-only and
  server-derived (D-219); every origin runs the SHARED validator; and the
  post-DNS dial guard means a granted origin can still never reach a private
  address. The adversarial review should attack the write path FIRST — try to
  smuggle a non-https origin, an origin with a path that reparses, an
  IP-literal, a rebinding hostname, and a body-supplied `tenant`/`admin` claim.
- **Revoke-prune fidelity.** Pruning the recorded requirement by matching the
  AS entry's `source_url` origin against the revoked set is the whole of
  constraint 3's live effect. If the match is loose (substring, no port
  normalisation), a revoke can silently keep stale data. The prune reuses the
  same origin normalisation the walker uses (`discovery.go:337`) — one
  normaliser, pinned by a test with a port-differing origin.
- **Concurrent write vs in-flight walk.** A revoke landing mid-`Discover` can
  produce a requirement record fetched under an allowance that no longer exists
  by the time it is recorded. Accepted and bounded: the record is stamped with
  the allowance generation it was walked under, and a record whose generation is
  stale is pruned at record time. Named in the plan so the adversarial review
  checks it rather than discovering it.
- **`stdio` connections have no allowance meaning.** Refused loudly rather than
  stored — otherwise a stored-but-inert field is a lie the diff renders.

## Glossary additions

- **OAuth discovery allowance** — the per-connection allow-list of public https
  origins the MCP OAuth-requirement discovery walker (D-297) may fetch
  cross-origin metadata from. Boot-declared in
  `mcp.servers[].oauth_discovery_allowed_origins` and — from this phase —
  revisioned on the agent-config connection descriptor and writable live over
  `agent_config.set_mcp_discovery_origins` (admin-only, server-derived
  authority). It is an allow-LIST, never a network hole: a granted origin is
  still refused at dial time if it resolves to a private / loopback address
  (the post-DNS `net.Dialer.Control` backstop). Revoking an origin takes effect
  on the live connection immediately AND prunes the requirement data that
  origin already produced. RFC §6.4, §6.16, D-297, D-300.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
      (the write + read path identity legs; a cross-tenant write is refused)
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent
      `SetOAuthDiscoveryOrigins` writes interleaved with `OAuthDiscoveryTarget`
      reads and `Discover` walks against ONE shared `Registry` under `-race`; no
      torn slice, no cross-connection bleed, no goroutine leak (D-025)
- [ ] **Integration test exists** (`test/integration/phase166_discovery_allowance_test.go`),
      wires real drivers end-to-end against the §17.8 spec-derived fixture,
      asserts identity propagation, covers ≥1 failure mode, runs under `-race`
- [ ] Wire changes complete: `make protocol-ts-gen-check` +
      `make protocol-docs-gen-check` green with regenerated artifacts committed
      (D-223 / D-209); `ProtocolVersion` unbumped (additive)
- [ ] §18 skill hygiene: grep `docs/skills/` for `surface: protocol` /
      `surface: console` / `surface: mcp` and update any playbook this verb
      makes stale, in the SAME PR
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry
      filed
