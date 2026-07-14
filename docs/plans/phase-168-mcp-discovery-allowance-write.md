# Phase 168 — Live MCP OAuth discovery-allowance write

> Part of the v1.14 wave (`docs/plans/wave-v114-coordination.md`). Decision:
> **D-302**. The discovery-allowance write half of HA-15, re-homed from the
> original v1.14 Phase 166 after two adversarial reviews restructured the wave.
> It lands ON the identity-keyed registries (Phase 167) and the credential-sink
> hardening (Phase 166).

## Summary

Phase 164 (D-297) shipped MCP OAuth-requirement discovery, but its RFC 8414
authorization-server hop is inherently cross-origin and requires an explicit
per-connection origin allowance — which today exists ONLY as the
restart-required yaml field `mcp.servers[].oauth_discovery_allowed_origins`
(`internal/config/config.go:1308`). A Protocol-driven consumer that adds MCP
connections at runtime has no way to grant it. Worse, discovery is INERT for
every runtime-added connection: the allowance is not carried from the add
request through to the live registry at all (a §17.1 cross-package wiring gap —
see the honest restatement below). This phase makes the allowance a first-class,
revisioned, diffable, rollback-able field on the agent-config connection
descriptor; adds ONE narrow admin-gated Protocol verb
(`agent_config.set_mcp_discovery_origins`) that writes it on the revision spine,
applies it to the identity-keyed live registry (Phase 167), AND takes effect on
rollback through a run-start reconcile leg; and ships the D-062 Console consumer
that closes the see-it-here / fix-it-there gap between the MCP Connections page
and the Agent Config page.

## RFC anchor

- RFC §6.4 — the MCP southbound edge (the discovery walker's allowance input and
  the identity-keyed registry that snapshots it).
- RFC §6.16 — the agent-config control plane (the revision spine: versioned,
  diffable, rollback-able).
- RFC §5.2 — what the Protocol exposes (the admin write verb + the additive
  descriptor field).
- RFC §6.15 — the audit posture (the admin-scope audit on the write, using the
  fail-closed ordering Phase 166 corrected).
- RFC §7 — the Console lens (the two-page consumer).

## Briefs informing this phase

- brief 09
- brief 14
- brief 11

## Brief findings incorporated

- **brief 09 §"What bifrost provides" (the OAuth-discovery option):** discovery
  "reduces operator config burden." A config-only, restart-required allowance
  re-imposes exactly that burden for a Protocol-driven consumer — for whom it is
  a wall (no yaml, no restart). This phase makes the allowance reachable from
  the same control plane that adds the connection.
- **brief 09 §"the dynamic-registration footguns":** fail loud on every branch.
  Applied to the WRITE path: a malformed origin, an unknown connection, and a
  boot-declared connection each fail loud with a distinct typed error; a grant
  that cannot be audited fails the call closed (using Phase 166's corrected
  audit ordering).
- **brief 14 §item 9 ("OAuth for HTTP servers"):** the MCP auth sequence is
  discovery-driven. This phase does NOT add a second discovery chain — the
  walker (`internal/tools/auth/discovery.go`) and its per-hop SSRF guardrails
  are untouched; it feeds the walker's existing `DiscoveryInput.AllowedOrigins`
  input (`discovery.go:167`) from a live, identity-keyed, revisioned source
  instead of a boot-frozen one.
- **brief 11 §"MCP Connections view":** the MCP Connections page is the
  operator's connection lens. Held with a hard boundary: the page is where the
  operator SEES `needs_allowance`, but NOT where the write lives — the write is
  single-homed on the Agent Config connection surface (where revisions/diff/
  rollback already render), which the page deep-links to. One write affordance,
  not two (§13).

## Findings I'm departing from (if any)

None on design. **One correction to the original v1.14 Phase 166's defect
statement (carried here honestly, WARN 8):** the earlier framing claimed
`MCPConnectionAttacher.Attach` (`internal/runtime/serve/mcp_attacher.go:79-87`)
DROPS `OAuthDiscoveryAllowedOrigins`. That is FALSE — nothing upstream carries
the field: `AttachRequest` has no such field, neither does
`agentcfg.MCPConnectionDescriptor` nor the wire descriptor. The consequence is
real (discovery is inert for every runtime-added connection), but the mechanism
is a §17.1 cross-package WIRING GAP — Phase 164 shipped the walker, Phase 92f
shipped the add path, and neither joined them. The correction matters because
the discredited "a regression test that fails against the pre-fix attacher"
discriminator was unachievable (pre-fix there is no field to send); the real
discriminator is a within-phase unit guard that the full descriptor →
`AttachRequest` → `config.MCPServerConfig` → identity-keyed registry carry
round-trips (see the acceptance criteria).

## Goals

- **Make the allowance revisionable (gap 1).**
  `agentcfg.MCPConnectionDescriptor` (`internal/agentcfg/agentcfg.go:220-243`)
  gains `OAuthDiscoveryAllowedOrigins []string` — a NON-SECRET field (an origin
  allow-list, not a secret), inheriting the whole spine for free: `SetRevision`,
  `Diff`, `Rollback`, `ListRevisions`.
- **Close the wiring gap end to end (gap 2, §17.6).** Add the field to
  `AttachRequest` (`internal/runtime/agentcfg/protocol/addconnection.go:71-93`)
  and carry it in `MCPConnectionAttacher.Attach` into the
  `config.MCPServerConfig` it builds — so a runtime-added connection's discovery
  is no longer inert. The discriminator is a within-phase round-trip unit test
  (descriptor → attach → the identity-keyed registry's `OAuthDiscoveryTarget`
  returns the allowance), NOT a pre/post-attacher comparison.
- **Add ONE narrow admin write verb.** `agent_config.set_mcp_discovery_origins`
  — request `{agent_id, name, allowed_origins[]}`, FULL REPLACE (the only
  semantic that diffs and rolls back cleanly). It writes a new revision under
  `lockAgent` carrying every sibling section forward (the D-283 guard), AND
  applies the set to the identity-keyed live MCP registry (Phase 167) so the
  very next discovery uses it.
- **Make the write LIVE via a registry mutator, and make REVOKE live and
  symmetric (gap 3).** The registry gains `SetOAuthDiscoveryOrigins`, modelled
  structurally on `SetRawHTMLTrust` — identity-mandatory (it takes the triple;
  Phase 167 keyed the registry), mutex-guarded, returning the prior set. Revoke
  is the load-bearing half: dropping an origin makes the next discovery refuse
  that hop AND PRUNES the recorded `oauth_requirement`'s authorization-server
  entries fetched from a revoked origin. **The prune builds a FRESH
  `oauth_requirement` and swaps the stored pointer under `r.mu.Lock()`** — the
  registry hands the requirement out by pointer (`registry.go:605`), so mutating
  it in place is a data race (WARN 10); the swap is pinned in the concurrent
  test.
- **Rollback / `set_revision` take effect LIVE via a run-start reconcile leg
  (FAIL 7).** A revisioned write with no live effect is the exact "changes the
  revision and not the runtime" silent half-write this phase (and D-300) rejects
  for a general patch verb — so it must not be reintroduced for rollback. A
  run-start allowance reconcile leg (beside `ReconcileConnections`,
  `internal/runtime/agentcfg/projection/projection.go:141`, now identity-scoped
  by Phase 167) reconciles each connection's LIVE allowance against its active
  revision at run start: a rollback past a grant REVOKES the origin live (and
  prunes as above). AC + named test:
  `TestReconcile_RollbackPastGrant_RevokesOriginLive`.
- **Reuse the shipped origin validator (hard constraint 2).**
  `validateDiscoveryOrigin` (`internal/config/validate.go:2276-2301`) is
  EXPORTED as `config.ValidateDiscoveryOrigin` and the existing unexported
  validator becomes a one-line caller. ONE implementation, two call sites (boot
  plus the Protocol write). No second parser.
- **Prove allowance ≠ SSRF bypass (hard constraint 2).** The walker's
  `net.Dialer.Control` hook (`discovery.go:260-281`) runs PER-DIAL,
  POST-DNS-RESOLUTION, so a runtime-granted origin whose hostname resolves to a
  private / loopback address is STILL refused. Named test.
- **Authorization is server-derived and `admin`-gated (hard constraint 4).**
  The route inherits the `AgentConfigHandler` `default:` arm
  (`internal/protocol/transports/stream/agentconfig_handler.go:133-155`) by NOT
  joining the session-safe (`:224-230`) or user (`:238-244`) exception maps.
  Identity from `resolveIdentity(r)` / `identityFromScope` — never the body
  (D-219). No new scope (D-284).
- **Audit the grant with Phase 166's corrected fail-closed ordering.** The write
  emits an admin-scope audit event (non-secret: agent, connection name, granted
  / revoked sets). On emit failure the CALL fails closed with NO observable
  state change — using the emit-then-apply / apply-then-compensate helper Phase
  166 established (NOT the pre-166 `handleSetRawHTMLTrust` lie).
- **Console consumer, same phase (D-062 + §13).** Resolved deliberately — see
  "Console consistency".

## Non-goals

- **No change to the discovery walker or its SSRF guardrails**
  (`internal/tools/auth/discovery.go` untouched).
- **No general `update_mcp_connection` patch verb** — `url` / `command` /
  `transport` / `oauth_provider` are consumed at ATTACH time; patching them
  without a re-attach is a silent half-write. The allowance is the ONE field
  re-read per discovery call.
- **No allowance-generation counter.** The original plan floated an "allowance
  generation" to bound a revoke landing mid-`Discover`. Dropped (WARN 11): each
  `Discover` already reads a per-call SNAPSHOT copy of the origins via
  `OAuthDiscoveryTarget`, so a revoke mid-walk produces at most ONE stale
  requirement record, which the next run-start reconcile prune corrects. The
  accepted bound (a single stale record until the next reconcile) is documented
  here rather than defended by a counter that would add concurrency surface for
  a self-healing race.
- **No OAuth-provider descriptor install** (Phase 169) and no OAuth flow
  execution / token custody (D-297 boundaries unchanged).
- **No boot-declared (yaml) connection editing.** The verb governs revisioned
  state only; a boot-declared connection's allowance is edited in `harbor.yaml`
  and applied by a restart (the Phase 156 boot-declared typed refusal).

## Acceptance criteria

- [ ] `agentcfg.MCPConnectionDescriptor` carries
      `OAuthDiscoveryAllowedOrigins []string` (non-secret, `omitempty`);
      round-trips through `SetRevision` / `Active` / `Diff` / `Rollback` on all
      three StateStore drivers; passes the
      `rebuild_completeness_test.go` reflection walk (D-283).
- [ ] **Wiring-gap closure (§17.6), within-phase discriminator:** the field is
      carried descriptor → `AttachRequest` → `config.MCPServerConfig` → the
      identity-keyed registry; a unit test adds a connection with an allowance
      over the Protocol and asserts `OAuthDiscoveryTarget` (triple-scoped)
      returns it. (No pre/post-attacher comparison — the field did not exist
      pre-phase.)
- [ ] `agent_config.set_mcp_discovery_origins` is registered in EVERY canonical
      home: `methods.go` const + `canonicalMethods` + `canonicalAgentConfigMethods`
      + `canonicalAgentConfigAdminMethods` (**and the prose counts at
      `methods.go:1219` / `:1290` are updated** — NIT 19), `singlesource.go`,
      `conformance.go`, `cmd/harbor-gen-protocol-docs/methods.go`; and
      `make protocol-ts-gen` + `make protocol-docs-gen` are re-run with the
      regenerated `wire-manifest.gen.json` + `docs/site/protocol/*.md`
      committed (D-223 / D-209 — all three lockstep gates green).
- [ ] The verb is **admin-gated**, test-pinned: a valid-identity-but-no-`admin`
      caller is rejected with `CodeScopeMismatch` before dispatch; an
      `agent_config:user`-scoped caller is ALSO rejected; authority is read from
      the verified ctx, never the body (D-219). Test:
      `TestSetMCPDiscoveryOrigins_RejectsNonAdminScope`.
- [ ] Every origin runs the SHARED `config.ValidateDiscoveryOrigin`; a
      non-https / path-bearing / IP-literal / empty origin fails loud with a
      typed `CodeInvalidRequest` naming the entry.
      Test: `TestSetMCPDiscoveryOrigins_RejectsMalformedOrigin`.
- [ ] **Allowance ≠ SSRF bypass:** a runtime-granted origin resolving to a
      private / loopback address is STILL refused at dial time.
      Test: `TestDiscovery_RuntimeGrantedOrigin_StillRefusesPrivateDial`.
- [ ] **The write is LIVE and identity-scoped:** after a successful call,
      `OAuthDiscoveryTarget` for that connection under the caller's triple
      returns the new set without restart/re-attach, and a DIFFERENT tenant's
      same-named connection is unaffected (relies on Phase 167 keying).
- [ ] **REVOKE is LIVE and symmetric:** removing an origin (a) refuses the next
      discovery hop with `needs_allowance`, AND (b) prunes the recorded
      `oauth_requirement`'s AS entries fetched from the revoked origin — by
      building a FRESH requirement and swapping the pointer under the lock (no
      in-place mutation of a handed-out pointer). Tests:
      `TestSetMCPDiscoveryOrigins_RevokePrunesRecordedRequirement`,
      `TestSetMCPDiscoveryOrigins_RevokePrune_NoPointerRace` (under `-race`).
- [ ] **Rollback past a grant revokes the origin LIVE (FAIL 7):** rolling the
      agent config back to a revision predating a grant revokes the origin on
      the live registry through the run-start reconcile leg. Test:
      `TestReconcile_RollbackPastGrant_RevokesOriginLive`.
- [ ] The write emits ONE admin-scope audit event and fails the CALL closed on
      emit failure with NO observable state change (Phase 166's corrected
      ordering). Test:
      `TestSetMCPDiscoveryOrigins_FailsClosedOnAuditEmitFailure`.
- [ ] Distinct typed errors: unknown connection name; a boot-declared (yaml)
      name (Phase 156's typed error); a `stdio`-transport connection (no HTTP
      401 edge — refused, not stored).
- [ ] **Console consumer (D-062), both pages, one write affordance** — see
      "Console consistency"; incl. the deep-link agent-context resolution (item
      12).
- [ ] `scripts/smoke/phase-168.sh` OK ≥ 3, FAIL = 0.
- [ ] `-race` green; coverage ≥ the stated target on every touched Go package.

## Files added or changed

- `internal/agentcfg/agentcfg.go` — the descriptor field.
- `internal/config/validate.go` — export `ValidateDiscoveryOrigin`; the existing
  validator becomes its caller.
- `internal/runtime/serve/mcp_attacher.go` — the `AttachRequest` →
  `config.MCPServerConfig` allowance carry (the wiring-gap closure).
- `internal/runtime/agentcfg/protocol/addconnection.go` — `AttachRequest` gains
  the field; `recordConnectionRevision` persists it.
- `internal/runtime/agentcfg/protocol/setdiscoveryorigins.go` (new) — validate
  → `lockAgent` → revision write → identity-keyed live registry apply →
  revoke-prune → audit emit (fail-closed helper).
- `internal/runtime/agentcfg/projection/projection.go` — the run-start allowance
  reconcile leg (FAIL 7), identity-scoped (Phase 167).
- `internal/tools/drivers/mcp/registry.go` — `SetOAuthDiscoveryOrigins`
  (triple-keyed, Phase 167) + the fresh-requirement-swap prune (WARN 10).
- `internal/protocol/methods/methods.go` — const + the three canonical sets +
  the prose counts.
- `internal/protocol/types/agentconfig.go` — request/response types +
  `AgentConfigMCPConnectionDescriptor.oauth_discovery_allowed_origins`.
- `internal/protocol/transports/stream/agentconfig_handler.go` — the route
  (admin-gated by omission), reusing `decode` / `assertIdentity` /
  `writeServiceError`.
- `internal/protocol/singlesource/singlesource.go`,
  `internal/protocol/conformance/conformance.go`,
  `cmd/harbor-gen-protocol-docs/methods.go` + the `typeindex.go` files.
- `web/console/src/lib/protocol/` (wire mirror + client method),
  `web/console/src/lib/agentconfig/state.svelte.ts` + the connections card
  (grant/revoke editor + the remove affordance),
  `web/console/src/lib/components/mcp-connections/McpDetailRail.svelte` (the
  `needs_allowance` deep-link + the agent-context resolution, item 12).
- `wire-manifest.gen.json` + `docs/site/protocol/{methods,types}.md`
  (regenerated).
- `test/integration/phase168_discovery_allowance_test.go` (new).
- `scripts/smoke/phase-168.sh` (new); `docs/plans/README.md`;
  `docs/decisions.md` (D-302); `docs/glossary.md`.

## Public API surface

```go
// Wire (additive):
//   agent_config.set_mcp_discovery_origins
//     req  {agent_id, name, allowed_origins[]}   // FULL REPLACE
//     resp {revision, granted[], revoked[], applied_live bool}
//   AgentConfigMCPConnectionDescriptor.oauth_discovery_allowed_origins []string

// internal/config
func ValidateDiscoveryOrigin(origin string) error  // exported; one implementation

// internal/tools/drivers/mcp — triple-keyed (Phase 167), internally
// synchronised (D-025); returns the prior set so the caller computes the
// revoked delta. Illustrative:
//   func (r *Registry) SetOAuthDiscoveryOrigins(ctx context.Context, name string, origins []string) (prev []string, err error)
```

## Test plan

- **Unit:** descriptor round-trip + the D-283 walk; the wiring-gap round-trip
  discriminator; the shared-validator origin table; the scope gate
  (no-scope / user-scope → `CodeScopeMismatch`); the typed-error table
  (unknown / boot-declared / stdio); audit fail-closed (no state change on emit
  failure); revoke-prune builds a fresh requirement (no pointer mutation).
- **Integration (`test/integration/phase168_discovery_allowance_test.go`) —
  binding per §17.1:** real drivers against Phase 164's spec-derived
  OAuth-challenging fixture — add-with-allowance → registry SEES it (the
  wiring-gap round trip); probe → the AS hop walks; `set_mcp_discovery_origins`
  revoking → next probe refuses AND the recorded requirement's AS entry is
  pruned (live, no restart); rollback past a grant → the origin is revoked live
  (FAIL 7); **cross-tenant:** a second tenant's same-named connection is
  unaffected by the first's grant/revoke (Phase 167 keying); identity
  propagation + a cross-tenant write refusal; `-race`.
- **Conformance (§17.8):** the fixture is Phase 164's committed RFC 9728 §3.2 +
  RFC 8414 §3.2 spec artifacts; any NEW fixture derives from the same artifacts
  / a captured transcript; a wrong-field mutation FAILS.
- **Concurrency / leak:** N≥100 concurrent `SetOAuthDiscoveryOrigins` writes
  interleaved with `OAuthDiscoveryTarget` reads, `Discover` walks, AND
  revoke-prunes against ONE shared identity-keyed `Registry` under `-race` — no
  torn slice, no pointer race on the pruned requirement, no cross-tenant bleed,
  no goroutine leak (D-025).

## Console consistency

Binding per CLAUDE.md §4.5 item 12 — cites `docs/design/console/CONVENTIONS.md`
(D-121) and `docs/design/console/PAGE-POLISH-PROCEDURE.md`. The ONE `(console)`
route group with unprefixed URLs (§1), the shared app shell (§2), the `ui/`
inventory (no hand-rolled primitives), the four-state `<PageState>` contract, the
unified `HarborClient` (no hand-rolled `fetch` — §4.5 item 5), and `tokens.css`
only (stylelint-enforced).

**The page split — resolved deliberately.** The operator SEES `needs_allowance`
on `/mcp-connections` (`McpDetailRail.svelte:296-344`, the status list at
`:336-342`), but every connection WRITE lives on `/agent-config`
(`AddConnectionCard.svelte` + `state.svelte.ts:901-942`):

1. **The write is single-homed on `/agent-config`** — the allowance is a field
   of the revisioned descriptor, and a revisioned write with no diff/rollback
   affordance beside it is dishonest. The connections card becomes a real
   EDITOR: per-connection grant/revoke + the `remove_mcp_connection` affordance
   (Phase 156's caller-less verb, `client.ts:1185-1196` — grep-verified). One
   write affordance per verb (§13).
2. **`/mcp-connections` gets a DEEP-LINK, never a second write form.** **Item 12
   resolution:** the MCP Connections detail rail carries NO agent context today
   (grep-verified: `agentId` appears only under `routes/(console)/agent-config/`),
   and — even with Phase 167 keying the runtime registry by identity — the
   connection→AGENT mapping (which agent revision declares this connection) is
   agent-config data, not registry data. So the deep-link handler reads the
   connection→agent mapping from the agent-config registry BEFORE rendering the
   link (`/agent-config?agent=<id>&connection=<name>&grant_origin=<origin>`); if
   the connection is boot-declared or not owned by any revisioned agent, it
   renders the yaml-edit copy and NO link. A Console vitest covers "connection
   not owned by the selected agent → no link, honesty copy."
3. **Boot-declared connections render "edit `harbor.yaml` and restart" copy and
   no link** (Phase 156's boot-declared refusal). Vitest pins both branches.

## Smoke script additions

`scripts/smoke/phase-168.sh` — classification `live-server` (the write verb is
served by the booted dev stack), with a `unit-tests` companion for the
SSRF/dial-guard leg:

- `agent_config.set_mcp_discovery_origins` present on the booted method surface
  (404/405/501 → SKIP on pre-168 builds).
- Grant against an UNKNOWN connection → typed loud error.
- Call WITHOUT admin scope → `CodeScopeMismatch`.
- Malformed origin → `CodeInvalidRequest`.
- Static: the method appears in `wire-manifest.gen.json` + the regenerated
  `docs/site/protocol/methods.md`.
- `go test -race` the registry mutator + the discovery dial-guard.
- Done-definition: `OK ≥ 3`, `FAIL = 0`.

## Coverage target

- `internal/runtime/agentcfg/protocol` (the service method + reconcile leg): 85%
- `internal/tools/drivers/mcp` (the mutator + the fresh-requirement prune): 85%
- `internal/agentcfg` (the field + diff arm): 85%
- `internal/config` (the exported validator): 90% (existing)
- Console: vitest on the editor state fold + the deep-link/boot-declared/agent-
  context branches.

## Dependencies

- **167** (the identity-keyed registries this writes to — the write and revoke
  must be triple-scoped, and `lockAgent` alone gives ZERO cross-tenant
  protection since it shards on `(scope, tenant, agent)` while the live registry
  is one map; Phase 167 closes that clobber — FAIL 5).
- **166** (the corrected fail-closed audit ordering this write copies).
- **164** (D-297 — the walker + registry state + the `needs_allowance` status).
- **92f** (the add verb + `ConnectionAttacher` seam whose wiring gap this
  closes), **156** (D-287 — the diff arm, the boot-declared refusal, and the
  caller-less `remove_mcp_connection`), **152** (D-283), **92h / 92i** (the
  Console panel + revision UX), **108m** (the MCP Connections page), **118**
  (D-223).

## Risks / open questions

- **The allowance is a security control being made runtime-writable.** Hard
  constraints 2 and 4 are named tests, not prose. The adversarial review attacks
  the write path first: non-https that reparses, a path-bearing origin, an IP
  literal, a rebinding hostname, a body-supplied `tenant`/`admin` claim.
- **Revoke-prune fidelity + the pointer race (WARN 10).** The prune matches the
  AS entry's `source_url` origin against the revoked set using the walker's
  origin normaliser (`discovery.go:337`) — one normaliser, port-normalised — and
  swaps a FRESH requirement pointer under the lock. Both are pinned (a
  port-differing origin test; the `-race` no-pointer-race test).
- **The self-healing mid-walk race (accepted, WARN 11).** A revoke landing
  mid-`Discover` yields at most one stale requirement record (the walk holds a
  snapshot copy of the origins), corrected at the next run-start reconcile
  prune. Documented bound; no generation counter.
- **92m collision (WARN 13).** The parked 92m (`docs/plans/README.md:1349`)
  plans an optional agent-bound `OAuth` block on the SAME `add_mcp_connection`
  request — a second Protocol-writable auth affordance. §13 forbids two parallel
  implementations. Ruling: this wave's provider surface (Phase 169's install +
  binding) is the one home; an unparked 92m must reuse it, not add a parallel
  block. Pointer notes are ACTUALLY written into `phase-92m` and `phase-92k` in
  this PR (D-302 records the ruling).

## Glossary additions

- **OAuth discovery allowance** — the per-connection allow-list of public https
  origins the MCP OAuth-requirement discovery walker (D-297) may fetch
  cross-origin metadata from. Boot-declared in
  `mcp.servers[].oauth_discovery_allowed_origins` and — from this phase —
  revisioned on the identity-keyed agent-config connection descriptor and
  writable live over `agent_config.set_mcp_discovery_origins` (admin-only,
  server-derived authority). An allow-LIST, never a network hole: a granted
  origin is still refused at dial time if it resolves private/loopback (the
  post-DNS backstop). Revoking an origin takes effect on the live connection
  immediately, prunes the requirement data that origin produced, and — on
  rollback — is revoked live through the run-start reconcile leg. RFC §6.4,
  §6.16, D-297, D-302.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] **If multi-isolation code paths changed: cross-tenant isolation test
      passes** — a second tenant's same-named connection is unaffected by the
      first's grant/revoke (Phase 167 keying)
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent writes / reads /
      walks / revoke-prunes against ONE shared identity-keyed `Registry` under
      `-race`; no torn slice, no pointer race, no cross-tenant bleed, no
      goroutine leak (D-025)
- [ ] **Integration test exists**
      (`test/integration/phase168_discovery_allowance_test.go`), wires real
      drivers end-to-end against the §17.8 spec-derived fixture, asserts
      identity propagation + the rollback-live-revoke leg, covers ≥1 failure
      mode, runs under `-race`
- [ ] Wire changes complete: `make protocol-ts-gen-check` +
      `make protocol-docs-gen-check` green with regenerated artifacts committed
      (D-223 / D-209); the `methods.go` prose counts updated (NIT 19);
      `ProtocolVersion` unbumped
- [ ] §18 skill hygiene: `docs/skills/use-the-harbor-protocol/SKILL.md`
      (`surface: protocol`) documents the new method; `observe-with-the-console`
      (`surface: console`) if the page copy changes materially — in the SAME PR
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: N/A — none departed (the one
      correction is to a prior phase's defect statement, recorded above)
