# Phase 216 — run-start connection re-establishment: attach what can be attached, report what cannot

## Summary

`projection.ReconcileConnections` only DETACHES. It reads the reconciling owner's attached
set and tears down what the active revision no longer declares (`projection.go:167-200`);
nothing in it — or anywhere on the run path — ATTACHES (`grep -c Attach
internal/runtime/serve/runloop.go` → **0**). So a connection a revision DECLARES but the
live registry does not carry never comes back: not after a restart, not after a rollback
that re-declares one. D-287's as-built note records this as deliberately deferred (the
"92o attach leg"), leaving the admin-gated `agent_config.add_mcp_connection` verb as the
only attach path. This phase builds that leg — at run start, for the reconciling owner, a
DECLARED-but-ABSENT connection is **re-established when it can be, and REPORTED when it
cannot**, through the same `mcpdrv.Attach` lifecycle, under the same `(tenant, agent)`
owner, behind every gate the add door applies. Zero new methods, zero new wire
request/response types; two additive canonical events.

**The interactive consent step is a CONFIG-TIME operator action and this phase neither
reimplements nor triggers it (§13).** It drives the existing admin-scoped
`agent_config.add_mcp_connection` / `agent_config.set_oauth_provider` verbs. This leg
re-establishes only what that earlier act already made self-sufficient.

### The credential plane is UNTOUCHED, and the reason is structural

**The MCP attach path never touches a token — for either provider driver family.**
`mcpdrv.Attach` resolves the OAuth PROVIDER INSTANCE **by name**
(`resolveOAuthBinding`, `attach.go:429` → `resolveProviderBinding`, `:545`, whose only
inputs are `(ms, mode, providers OAuthProviderResolver)`); it never calls
`provider.Token`. The bearer is minted PER CALL onto the call's ctx, in
`Provider.resolveBearerCtx` (`mcp.go:1452-1472`) and `resolveInjection` (`:1493`). The
persisted binding is likewise a NAME: `agentcfg.MCPConnectionDescriptor.OAuthProvider` is
godoc'd *"NON-SECRET — a provider NAME selecting a config-declared acquisition strategy;
the secret stays on the provider"* (`agentcfg.go:232-237`), and D-303's Protocol-installable
descriptor is zero-URL `{name, credential_broker, scopes?}`. **So a run-start re-attach
cannot initiate a flow, hold, refresh, mint, or exchange anything — there is no token step
in the code it runs.** The phase is therefore NOT limited to "connections whose bindings
are already resolvable"; every declared binding is re-resolvable, because resolving one is
a name lookup plus the boot-declared `AllowedDownstreamHosts` gate (`attach.go:568-577`).

**Correction to a premise this plan was handed, recorded because it matters.** It is NOT
true that Harbor "never runs the OAuth flow, never holds or refreshes a token." That
describes a `tokenexchange`-only DEPLOYMENT, not Harbor. D-271 item 2 is explicit: *"A
source is EITHER interactive (`oauth2`) or brokered (`tokenexchange`), declared in config —
no dual path."* Both drivers ship and both are blank-imported in the production aggregator
(`internal/drivers/prod/prod.go:161`, `:168`). They differ exactly on durability:

| Driver | Who runs the flow | Token custody | Survives restart |
|---|---|---|---|
| `oauth2` (interactive) | **Harbor** — full authorization-code + PKCE + RFC 7591 (`drivers/oauth2/oauth2.go:11-19`) | **Persisted**, AES-256-GCM-sealed, via `CompleteFlow` → `p.store.Put` (`provider.go:369`; refresh at `:676`) → `stateStoreTokenStore.Put` under `tools.auth.access.…` / `tools.auth.refresh.…` (`tokenstore.go:26-47`, `:235`, `:253`) | **Yes on sqlite/postgres, no on inmem** — durability is the §9 state driver (`internal/state/drivers/{inmem,sqlite,postgres}`) |
| `tokenexchange` (brokered) | Nobody in Harbor — RFC 8693 pull at token-miss time | **Never persisted.** TTL-cached in memory, single-flight; *"`store.Put` is NEVER called"* (`tokenexchange.go:26-30`, `:555-559`, `:931`); the pulled org credential is likewise memory-only (`credsource/credsource.go:18,30`, `drivers/remote/remote.go:48`) | N/A — nothing to survive; the next call re-pulls |

D-287 call 3 confirms the `oauth2` durability from the other side (*"Agent-bound sealed
tokens are NOT deleted on remove: re-add reuses completed consent"*; its as-built calls the
retention *"structurally guaranteed"*).

**Neither branch changes this phase's scope**, because the difference lands one layer
later than the attach. Where a consent is genuinely gone (inmem driver, revoked grant,
expired refresh, or a broker answering `consent_required`) the re-attach still SUCCEEDS and
the shortfall surfaces on the ALREADY-SHIPPED path: the first tool call's `provider.Token`
returns a typed `*auth.ErrAuthRequired`, the runtime emits `tool.auth_required`
(`provider.go:455`, `:559`) and parks on the unified pause/resume primitive — and D-271
item 3 routes the brokered driver onto the *same* sentinel. That is the reconnect surface
the operator's menu already consumes; this phase neither duplicates nor touches it (§13).

**One shape IS an exception, and it is not OAuth.** A connection authenticated by
operator-supplied static `Headers` cannot be re-established at all, because the headers are
secret and never persisted (`addconnection.go:88-90`, `:474-476`). That is the phase's one
genuinely non-re-establishable case; it is handled by AC 4.3 and reported, never papered
over.

## RFC anchor

- RFC §6.4 — tool catalog and transports (the MCP driver, the registry the attach path
  registers into, and the catalog the attached server's tools land on).
- RFC §6.16 — the Agent Registry (the `(tenant, agent)` owner tag the leg reconciles
  under; `agent_id` is registration metadata, never an isolation key).
- RFC §6.13 — the typed event bus (the two additive lifecycle events the leg emits).
- RFC §7 — the Console as a Protocol client (the connection surface an operator reads;
  the leg changes what `mcp.servers.list` reports after a restart).

## Briefs informing this phase

- brief 03 — tools + integrations (the one-catalog bare-name model the leg must not
  widen; the MCP transport auto-detect knob the re-dial inherits).
- brief 09 — MCP OAuth from bifrost (the credential-custody rules the re-attach must
  re-enforce rather than assume, and the identity-mandatory / concurrent-reuse bars).
- brief 14 — MCP client/host compliance (the host-side connection lifecycle, and the
  binding wording rule this plan applies to its own restart-survival claim).

## Brief findings incorporated

- **brief 03 §5 "Sharp Edges Harbor Must Avoid"** — *"Harbor picks one architecture and
  bakes the correction in"* (no parallel modes). The leg does NOT build a second attach
  implementation: it calls the SAME `mcpdrv.Attach` (`internal/tools/drivers/mcp/attach.go:135`)
  through the SAME production concrete (`serve.MCPConnectionAttacher`,
  `internal/runtime/serve/mcp_attacher.go:120`) the admin verb drives, so every gate —
  `CheckServerIDUnambiguous` (`attach.go:152`), the reserved-`_meta` check
  (`attach.go:433-441`), `resolveOAuthBinding` (`:429`), `resolveToolOAuthBindings` (`:455`),
  `resolveInjectionBinding` (`:485`), the same-name owner conflict (`:256-258`) — runs
  identically without being re-implemented.
- **brief 03 §5 "URL connections require explicit headers for auth (no implicit env
  passthrough)"** — read forward, this is exactly why the leg CANNOT reconstruct a
  header-authenticated connection: the headers are explicit, caller-supplied, and never
  persisted (`AttachRequest.Headers` godoc, `addconnection.go:88-90`). The plan states the
  bound rather than papering over it.
- **brief 09 §"What Harbor must add" item 3** (*"Admin-scope authz on agent-bound flows"*)
  and **item 4** (*"Identity-mandatory enforcement … fail closed on missing components"*).
  The leg attaches ONLY what an admin already wrote through the admin-gated door
  (`agentconfig_handler.go:149-156` — the `default:` arm requires `auth.ScopeAdmin`), under
  the owner that admin's revision owns; it never mints authority of its own, and a missing
  owner component is refused loud by the existing `ErrRuntimeAddOwnerMissing`
  (`mcp_attacher.go:128-131`).
- **brief 09 §"What Harbor must add" item 7** (the D-025 contract: *"no cross-identity
  bleed, no cross-agent bleed, no scope confusion … Test mandatory"*). The re-attacher is a
  compiled artifact with new mutable per-connection state (the failure-backoff table); it
  gets an N=128 two-owner concurrent run against ONE shared instance under `-race`.
- **brief 14 §4 "What Harbor can honestly claim"** (the binding wording rule: never state
  an unscoped claim; state the scoped one and let the harness substantiate it). Applied to
  this phase's own headline: the claim is **"a declared connection whose descriptor is
  self-sufficient is re-attached at run start"**, NOT "connections survive restart". A
  header-authenticated connection does not survive, and the plan says so in the goals, in
  an acceptance criterion, and in a test.

## Findings I'm departing from (if any)

- **D-287's as-built deferral of the attach leg.** D-287 records: *"the run-start
  reconciliation (`projection.ReconcileConnections`, 92o) did NOT exist. Phase 156
  therefore BUILDS the run-start reconcile mechanism as the home for its detach leg,
  **DETACH-ONLY** (attaching a declared-but-absent server — the restart-survival 92o attach
  leg — stays deferred; the live add verb is the attach path)."* This phase ENDS that
  deferral. It is not a reversal: D-287 deferred the leg, it did not rule against it, and
  D-287's own accepted-window note names its absence as the reason for both accepted
  windows (*"Two accepted windows **until the attach leg lands**"*). D-361 records the
  supersession of the deferral clause only; every other D-287 call (process-global
  catalog/registry, loud mid-run failure over refcount/drain, token retention on re-add) is
  preserved verbatim.
- **D-287's first accepted window is CLOSED by this phase, not inherited.** D-287 accepts
  that *"a reconcile racing a concurrent re-add of the same name by the SAME owner can
  detach the freshly re-added server (stale declared-set read — heals at the next add or
  restart)"*. With an attach leg, "heals at the next add or restart" becomes "heals at the
  next run start", and the plan additionally closes the same-shaped window on its OWN leg
  with an under-lock re-check (AC 7). Recorded in D-361 rather than left as an inherited
  note.
- No brief finding is departed from.

## Goals

- A connection the reconciling owner's active revision DECLARES, but that the live MCP
  registry does not carry under that owner, is attached at run start — the symmetric twin
  of the detach that already ships, in the same function, through the same seam shape the
  provider leg already uses (`ReconcileOAuthProviders`, `projection.go:321-364`, which
  already installs AND uninstalls).
- Restart survival and rollback-re-declare both work through ONE mechanism with N
  triggers, never two code paths.
- Every gate the admin add door applies is re-applied at run start against the CURRENT
  boot policy — not the policy in force when the revision was written. A gate that has
  since tightened refuses the re-attach.
- **The credential plane is untouched, as a stated property rather than a policy the leg
  enforces.** The binding is a NAME, the token step is per-call, and the attach path has no
  token step at all — so the leg mints, holds, refreshes, and exchanges nothing, and
  initiates no flow.
- **What cannot be re-established is REPORTED, not silently absent.** An unreachable or
  refused server never fails the run and never hangs its start; each outcome is a typed
  classification on a canonical event an operator surface can read.
- Two concurrent runs for the same owner attach the connection exactly once.
- Every guard is mutation-verified to turn a smoke `OK` into a `FAIL` — never a SKIP.

## Non-goals

- **User-scope MCP connections.** The scope this file previously carried is REJECTED and
  not re-attempted here. See "Risks / open questions" for why, and for what the operator's
  actual follow-on intent (users picking from ALREADY-CONFIGURED capabilities) should ride
  instead.
- **Any new Protocol method, wire request/response type, or error code.** The leg is
  internal Go plus two additive canonical event types. `ProtocolVersion` is unchanged.
- **Any new field on `agentcfg.MCPConnectionDescriptor`** (`internal/agentcfg/agentcfg.go:220-263`).
  The leg attaches from the descriptor as it stands today; if a field is missing, the
  re-attach fails loud rather than the descriptor growing to carry a secret.
- **Persisting attach-time `Headers`.** Not now, not behind a flag. They are secret
  (`addconnection.go:88-90`, `:474-476`) and the revision spine is a Protocol-readable,
  diffable, rollback-able store. A header-authenticated connection is not restart-survivable
  and this phase does not make it so.
- **Initiating, completing, or re-driving ANY OAuth flow.** The interactive consent gate is
  a config-time operator action driving existing admin-scoped verbs; the per-call
  `*auth.ErrAuthRequired` → `tool.auth_required` → pause cycle is the shipped reconnect
  surface. This leg calls neither.
- **Parking a run on the unified pause/resume primitive for an `auth_required` re-attach.**
  The add verb parks awaiting an ADMIN's consent (`addconnection.go:145-148`,
  `ErrAuthRequired`). A run-start reconcile has no admin request to resume and no consenting
  principal; parking a user's run on an admin's OAuth consent would be a new pause shape,
  not a use of the existing one. There is also nothing to hand it to: **the
  resume-completes-attach continuation is NOT shipped** — `addconnection.go:132-136` records
  *"the resume continuation that re-drives the attach to online is not yet implemented —
  resume currently only releases the pause (tracked in issue #375)"*, and phases 92m / 92n
  are both `Pending` in `docs/plans/README.md`.
- **Building 92m / 92n.** The add-flow OAuth parking and the resume-completes-attach bridge
  remain parked. This phase does not unpark them, does not depend on them, and its design
  must remain correct whether or not they later land.
- **Re-attaching a boot-declared (yaml) server.** Boot servers carry the zero owner
  (`attach.go:78-83`), are excluded from the owner view by construction
  (`registry.go:616-630`), and are attached by the boot loader. The leg never touches them.
- **Detaching anything.** The detach half is shipped and unchanged.
- **A forced-reconcile or `attach_now` Protocol verb.** Explicitly rejected in D-339 and
  not reopened.

## Acceptance criteria

- [ ] **AC 1 — the leg exists and is symmetric.** `projection.ReconcileConnections` gains
      an ATTACH pass beside its detach pass, driven by a new driver-agnostic
      `projection.ConnectionReattacher` seam, mirroring `ReconcileOAuthProviders`'
      uninstall-then-install shape (`projection.go:337-362`). A **nil** reattacher yields
      today's detach-only behaviour byte-for-byte (the backward-compatible path a driver
      without an attacher gets). Detach runs FIRST, so a name being replaced is torn down
      before the attach pass considers it.
- [ ] **AC 2 — the ordering against the sibling legs is pinned by test.**
      `RunLoopDriver.reconcileConnections` runs the provider reconcile first
      (`runloop.go:759-769`), then connections (`:772`), then the discovery-allowance
      re-apply (`:788-799`). The attach pass sits INSIDE the connections leg, so (a) a
      provider the same revision declares is installed before a connection binds it, and
      (b) a freshly attached connection receives its `oauth_discovery_allowed_origins` from
      the allowance leg in the same run start. Both orderings are asserted, not assumed.
- [ ] **AC 3 — the owner boundary holds.** The attach pass runs under
      `auth.Owner{Tenant: id.TenantID, Agent: agentID}` (`projection.go:184`) and the
      registration is stamped with that owner (`attach.go:255`, `AttachDeps.Owner`
      `:74-80`). A run for owner A never attaches, replaces, or observes owner B's
      registration, and never touches a boot-declared name. Asserted cross-owner within one
      tenant AND across tenants.
- [ ] **AC 4 — the credential plane resolves exactly three ways, each pinned.**
      1. **`oauth_provider` (name binding): RESOLVED, for BOTH driver families.**
         `resolveOAuthBinding` (`attach.go:429`) → `resolveProviderBinding` (`:545`) is a
         pure function of `(ms, mode, resolver)`; every authority it consults is
         boot-declared — the provider's `AllowedDownstreamHosts` (`:568-577`), the
         stdio/no-URL refusal (`:551-553`), the static-`Authorization` conflict
         (`:555-559`). No admin request context and **no token** participates, so the
         run-start resolution is byte-identical to the add door's. Pinned by test for the
         interactive (`oauth2`) AND brokered (`tokenexchange`) shapes, so a later reader
         cannot conclude one of them needs an interactive leg at attach.
      2. **`injection` (per-user credential-injection mapping): RESOLVED ONLY WHILE THE
         BOOT OPT-IN IS ON.** `resolveInjectionBinding` (`:485`) is equally pure, but the
         mapping reached the spine through the fail-closed DEV-ONLY `tools.allow_wire_injection`
         opt-in (`config.go:1048-1068`, `wireinjectiondescriptor.go:52`,
         `ErrWireInjectionNotAllowed`). The leg re-evaluates the LIVE effective opt-in and
         REFUSES to rebuild a descriptor carrying `injection` when it is off — the exact
         kill-switch shape `OAuthProviderInstaller.build` already ships
         (`ErrWireDescriptorDisabled`, `oauth_provider_installer.go:113-125`), threaded the
         same way (`serve.go:499-507`: *"the reconcile kill-switch, so a wire provider
         persisted while it was on is not rebuilt after a restart with it off"*).
      3. **static `Headers`: REFUSED BY CONSTRUCTION — they do not exist to resolve.**
         Never persisted, so a re-attach dials without them. A server that required them
         answers `401` and the re-attach fails loud + classified; a server that did not
         require them attaches correctly. There is no third outcome in which the runtime
         believes a header is present that is not.
      `tool_oauth_providers` is **not reachable** from a revision — the field is absent from
      `agentcfg.MCPConnectionDescriptor` (`agentcfg.go:220-263`) and exists only in boot
      yaml — so per-tool overrides are structurally out of the leg's reach. An inline wire
      `oauth` block is likewise unreachable: the add door turns it into an
      `agentcfg.OAuthProviderDescriptor` in the providers section
      (`addconnection.go:210-222`), which `ReconcileOAuthProviders` already owns and already
      kill-switches.
- [ ] **AC 5 — the stdio RCE gate is re-applied against the CURRENT boot allowlist.** A
      declared stdio descriptor whose `command[0]` is absent from the live
      `tools.mcp_add_connection.stdio_allowlist` (`serve.MCPAddStdioAllowlist`,
      `mcp_attacher.go:273-278`) is REFUSED at run start with a typed error and never
      spawned — even though it was allowlisted when the revision was written. This is the
      leg `gateStdioConnectionCommands` was written for: its godoc already states that both
      revision doors are gated so a refused command *"cannot enter through the other and sit
      there as input for any future attach-from-revision leg"* (`addconnection.go:439-453`;
      D-350 item 5). An empty allowlist refuses every stdio re-attach.
- [ ] **AC 6 — failure never fails the run and never hangs it.** A dial / handshake /
      discovery failure is logged at Error, emitted as `mcp.connection.reattach_failed`
      (SafePayload, reason scrubbed through the existing `safeReason`,
      `addconnection.go:769-788`), does NOT abort the sweep (errors joined, mirroring
      `projection.go:191-196`), and does NOT fail the run — the detach leg's shipped posture
      (`runloop.go:773-780`). **Each attach runs under its own bounded
      `context.WithTimeout`, and the sweep under a bounded total.** This is load-bearing,
      not hygiene: `Provider.Connect` carries NO internal timeout — it inherits the caller's
      ctx (`mcp.go:545-568`) — so an unresponsive declared server would otherwise stall
      every run's start until run cancellation.
- [ ] **AC 7 — idempotent under concurrency, reusing the shipped mechanism.** Two
      concurrent run starts for the same owner attach the connection EXACTLY ONCE. The
      reattacher re-reads the live registry **inside** the attacher's existing
      whole-attach lock (`mcp_attacher.go:158-159`) and no-ops when the name is already
      registered under the reconciling owner, closing the stale-view window between
      `AttachedSources` and the attach. Phase 198 / D-339's same-name replace
      (`attach.go:234-279`) is REUSED, not re-implemented, and no new lock is introduced.
- [ ] **AC 8 — the same-name owner conflict is inherited verbatim and classified as
      terminal.** A declared name already registered to a DIFFERENT owner is refused by the
      shipped `ErrConnectionNameOwnerConflict` (`attach.go:256-258`, `:413-418`). The
      run-start leg does not soften it, does not rename, and does not shadow: it classifies
      the refusal as NON-RETRYABLE (it cannot heal by re-dialling; only the other owner's
      detach or a rename fixes it) and emits it once per attempted descriptor rather than
      on every run start. `ErrAmbiguousServerID` (`registry.go:410-420`) is classified the
      same way. This is D-301's stated bound made VISIBLE at run start rather than only at
      add time — the trust boundary is unchanged.
- [ ] **AC 9 — retry is bounded and the first failure is loud.** Per `(owner, name)`
      exponential backoff with a cap, held on the reattacher concrete under its existing
      mutex and documented internally-synchronised (§5 / D-025). Reset on success or when
      the attempted descriptor differs from the last-failed one (so an operator's edit
      retries immediately). The FIRST failure in a window emits the event; suppressed
      attempts log at Debug and are counted, and the count rides the next emitted event —
      bounded, never silent (§13).
- [ ] **AC 10 — restart survival is proven end to end.** A connection added through
      `agent_config.add_mcp_connection` against a real MCP fixture server, then a fresh
      runtime booted on the SAME state store: the first run start re-attaches it, its tools
      are back in the projected planner catalog, and `mcp.servers.list` reports it. Proven
      against a spec-derived fixture (§17.8), not a stub.
- [ ] **AC 11 — rollback-re-declare is proven end to end.** Add → remove (detach at next
      run start) → rollback to the revision that declared it: the following run start
      re-attaches it through the SAME leg. One mechanism, N triggers.
- [ ] **AC 12 — two additive canonical events, no other wire movement.**
      `mcp.connection.reattached` and `mcp.connection.reattach_failed`, both reusing the
      EXISTING `agentcfg.MCPConnectionLifecyclePayload` shape verbatim
      (`internal/agentcfg/events.go:317-343`) — no new payload type. Registered in
      `events.go`'s `init()` list (`:130-148`) and in the docs generator's join table
      (`cmd/harbor-gen-protocol-docs/events.go:98`); `make protocol-ts-gen` +
      `make protocol-docs-gen` re-run and committed (D-223 / D-209). A NEW event type is
      required rather than reusing `mcp.connection.added` / `.failed`, whose godoc binds
      them to *"an admin add"* (`events.go:56-72`) — reusing them would make a shipped
      godoc false. A consumer distinguishes the two families structurally as well: the
      admin add's `Author` quadruple has an EMPTY `RunID` (`addconnection.go:174`), the
      reconcile's carries the reconciling run's.
- [ ] **AC 13 — no new Protocol method, request/response type, or error code;
      `ProtocolVersion` unchanged.**
- [ ] **AC 15 — the leg is credential-neutral, asserted negatively.** A test drives a
      run-start re-attach of a connection bound to an interactive (`oauth2`) provider whose
      `TokenStore` is EMPTY, and of one bound to a brokered (`tokenexchange`) provider whose
      broker is unreachable. **Both re-attaches SUCCEED**, and neither
      `OAuthProvider.Token`, `InitiateFlow`, `CompleteFlow`, `TokenStore.Get/Put`, nor any
      broker round-trip is invoked during the attach (counted on a recording provider). The
      shortfall surfaces later, on the FIRST TOOL CALL, as the shipped typed
      `*auth.ErrAuthRequired` → `tool.auth_required` → pause cycle — asserted, and asserted
      to be reached through the EXISTING path with no code added by this phase (§13). This
      is the acceptance centrepiece for "the credential plane does not widen".
- [ ] **AC 16 — every non-re-establishable outcome is REPORTED with a distinct typed
      class.** The closed set, each emitted on `mcp.connection.reattach_failed` with a
      stable class and each pinned by test: transport/handshake failure (includes the
      header-authenticated case, AC 4.3), `ErrReattachStdioNotAllowed`,
      `ErrReattachInjectionDisabled`, `ErrOAuthBinding` (unknown provider name or a host
      absent from the boot allow-list), `ErrConnectionNameOwnerConflict`, and
      `ErrAmbiguousServerID`. A silently-absent connection is a bug (§13): the smoke asserts
      that inducing each class produces an event, not merely a log line.
- [ ] **AC 14 — mutation-verified.** `scripts/smoke/phase-216.sh` shows `OK ≥ 10`,
      `SKIP = 0`, `FAIL = 0` against a live preflight build, and turning off any single
      guard turns a corresponding `OK` into a `FAIL`, never into a SKIP.

## Files added or changed

```text
internal/runtime/agentcfg/projection/
  projection.go                                  # ConnectionReattacher seam; ReconcileConnections
                                                 #   gains the attach pass (detach first, then attach)
  reconcile_attach_test.go                       # NEW — attach pass, ordering, owner scoping,
                                                 #   nil-reattacher backward compatibility
  reconcile_test.go                              # existing detach assertions, unchanged behaviour re-pinned
internal/runtime/serve/
  mcp_attacher.go                                # Reattach(ctx, owner, desc); the injection kill-switch;
                                                 #   the stdio re-gate; under-lock idempotency re-check;
                                                 #   the bounded per-connection ctx; the backoff table
  mcp_attacher_reattach_test.go                  # NEW — gates, kill-switch, backoff, N=128 two-owner
  runloop.go                                     # wire the reattacher into reconcileConnections;
                                                 #   the two event emits + the loud-but-non-fatal logging
  serve.go                                       # thread the effective allow_wire_injection opt-in +
                                                 #   the boot stdio allowlist into the attacher (the
                                                 #   serve.go:503-507 precedent, one line lower)
  runloop_reattach_test.go                       # NEW — the leg's wiring + failure posture
internal/agentcfg/
  events.go                                      # two additive canonical event types + init() registration
cmd/harbor-gen-protocol-docs/
  events.go                                      # the two join rows (lockstep test fails without them)
harbortest/devstack/devstack.go                  # the D-094 twin: wire the reattacher (§17.6)
web/console/src/lib/protocol/wire-manifest.gen.json  # REGENERATED (D-223) — never hand-edited
docs/site/protocol/events.md                     # REGENERATED (D-209) — never hand-edited
test/integration/
  phase216_run_start_attach_test.go              # NEW
  wave_v124_test.go                              # NEW — the wave-end E2E (see Test plan)
scripts/smoke/phase-216.sh                       # real assertions replacing the skeleton
docs/plans/phase-216-run-start-connection-attach.md   # THIS FILE (renamed from -user-scope-mcp-connections)
docs/plans/README.md                             # the phase row + detail block rewritten to this scope
docs/decisions.md                                # D-361
docs/glossary.md                                 # "run-start attach leg", "reattach backoff"
docs/skills/use-the-harbor-protocol/SKILL.md     # §18 (surface: protocol) — the connection lifecycle
                                                 #   paragraph gains restart survival + its bound
```

**Generated-file ownership.** Phase 215 owns `wire-manifest.gen.json` and
`docs/site/protocol/*.md` in Stage 1 (`wave-v124-coordination.md:78-83`); this phase rebases
on it and re-runs both generators. It adds no wire TYPE, so its manifest delta is confined
to the canonical-event list — the smallest possible conflict surface among the three Stage-2
phases.

## Public API surface

```go
// internal/runtime/agentcfg/projection

// ConnectionReattacher is the driver-agnostic seam the run-start ATTACH pass uses to
// bring a DECLARED-but-ABSENT runtime-added MCP server back under the reconciling owner.
// It is the symmetric twin of ConnectionDetacher and pairs with it in the same
// ReconcileConnections call; the concrete (wired at the cmd/harbor + devstack boundary)
// drives the SAME mcpdrv.Attach lifecycle the admin add verb drives. Keeping it an
// injected interface preserves this package's §4.4 boundary: the projection imports no
// concrete MCP driver.
type ConnectionReattacher interface {
    // Reattach attaches desc under owner. It is IDEMPOTENT: a name already registered
    // under owner is a no-op (the concrete re-checks the live registry under its own
    // attach lock, closing the stale-view window between the caller's AttachedSources
    // read and this call). Every gate the admin add door applies is re-applied here
    // against CURRENT boot policy. Errors are classified by the caller through
    // errors.Is; a nil error means the server is live and its tools are registered.
    Reattach(ctx context.Context, owner auth.Owner, desc agentcfg.MCPConnectionDescriptor) error
}

// ReconcileConnections gains one parameter. A nil reattacher is the backward-compatible
// detach-only path. Detach runs first, then attach.
func ReconcileConnections(
    ctx context.Context,
    reg agentcfg.Registry,
    agentID string,
    id identity.Quadruple,
    detacher ConnectionDetacher,
    reattacher ConnectionReattacher,
    bootDeclared map[string]struct{},
) (detached, attached int, err error)

// internal/runtime/serve

// ErrReattachStdioNotAllowed — a declared stdio descriptor's argv[0] is absent from the
// CURRENT boot allowlist. The revision was written while it was allowlisted; boot policy
// has since tightened. Fail-closed: never spawned. Callers compare with errors.Is.
var ErrReattachStdioNotAllowed = errors.New(...)

// ErrReattachInjectionDisabled — a declared descriptor carries an `injection` mapping but
// the fail-closed tools.allow_wire_injection opt-in is OFF. The dev opt-in is a
// kill-switch: a mapping persisted while it was on is not rebuilt after a restart with it
// off. The twin of ErrWireDescriptorDisabled on the provider side.
var ErrReattachInjectionDisabled = errors.New(...)

func (a *MCPConnectionAttacher) Reattach(ctx context.Context, owner toolauth.Owner,
    desc agentcfg.MCPConnectionDescriptor) error
```

The backoff table is package-internal state on `*MCPConnectionAttacher`, guarded by its
existing `mu` and documented "internally synchronised" per §5.

## Test plan

- **Unit:**
  - `TestReconcileConnections_AttachesDeclaredButAbsent` — the core leg.
  - `TestReconcileConnections_NilReattacherIsDetachOnly` — the backward-compatible path,
    byte-for-byte today's behaviour.
  - `TestReconcileConnections_DetachRunsBeforeAttach` — a name dropped and re-declared in
    one revision transition is torn down before the attach pass considers it.
  - `TestReconcileConnections_AttachPassIsOwnerScoped` — owner A's sweep never attaches
    under B's owner; a boot-declared name is never a candidate.
  - `TestReconcileConnections_AttachErrorsAreJoinedNotFatal` — one refusing server does not
    strand the rest; `ErrReconcileRead` still fails loud (`projection.go:172-174`).
  - `TestMCPConnectionAttacher_Reattach_StdioReGatedAgainstCurrentAllowlist` — the AC-5
    tightening case, plus the empty-allowlist refusal.
  - `TestMCPConnectionAttacher_Reattach_InjectionKillSwitch` — a persisted `injection`
    descriptor is refused with `ErrReattachInjectionDisabled` when the opt-in is off, and
    attaches when it is on. Mirrors the shipped provider-side kill-switch test.
  - `TestMCPConnectionAttacher_Reattach_OAuthProviderBindingResolves` — the boot-declared
    `AllowedDownstreamHosts` gate fires identically at run start (host on the list attaches;
    host off it is refused with `ErrOAuthBinding`). Run as a table over BOTH driver
    families (`oauth2`, `tokenexchange`) so the name-only resolution is proven for each.
  - `TestMCPConnectionAttacher_Reattach_IsCredentialNeutral` — the AC-15 negative
    assertion: an empty `TokenStore` (interactive) and an unreachable broker (brokered)
    both still attach, with a recording provider proving `Token` / `InitiateFlow` /
    `CompleteFlow` / `Get` / `Put` call counts are all ZERO across the attach.
  - `TestMCPConnectionAttacher_Reattach_ShortfallSurfacesOnFirstCallNotOnAttach` — after a
    credential-less re-attach, the first `CallTool` returns the shipped typed
    `*auth.ErrAuthRequired` and `tool.auth_required` is emitted, through the existing path
    with no code added here.
  - `TestReattach_FailureClassesAreDistinctAndAllReported` — the AC-16 closed set: each
    induced class emits `mcp.connection.reattach_failed` carrying its own stable class.
  - `TestMCPConnectionAttacher_Reattach_HeaderAuthenticatedServerFailsLoud` — the AC-4.3
    bound, made a test rather than a caveat: a fixture requiring a header 401s and the
    re-attach fails classified, with nothing half-registered.
  - `TestMCPConnectionAttacher_Reattach_SameNameOtherOwnerRefusedTerminal` — the inherited
    `ErrConnectionNameOwnerConflict`, classified non-retryable and emitted once.
  - `TestMCPConnectionAttacher_Reattach_AlreadyRegisteredUnderOwnerIsNoOp` — the AC-7
    under-lock re-check; the live provider instance is unchanged (no transport churn).
  - `TestMCPConnectionAttacher_Reattach_BoundedContext` — a fixture that never answers the
    handshake: the attach returns within the bound and the sweep completes. Pins the
    `mcp.go:545-568` no-internal-timeout finding.
  - `TestMCPConnectionAttacher_Reattach_BackoffBoundsRetries` — a permanently-failing
    server is dialled a bounded number of times across N run starts; the first failure
    emits, the rest are counted; an edited descriptor resets the window.
  - Event-shape tests: both new types are registered (`events.RegisterEventType`), both
    payloads are `SafePayload`, and a sentinel secret placed in the descriptor's URL
    userinfo appears NOWHERE in either emitted payload (`safeReason` + `scrubReasonSecrets`,
    `addconnection.go:790-808`).
- **Integration:** `test/integration/phase216_run_start_attach_test.go` — the REAL
  `mcp.Registry`, the REAL `tools.Catalog`, the REAL agent-config registry over a REAL
  state store, the REAL `serve.MCPConnectionAttacher` / `MCPConnectionDetacher`, a real
  in-memory bus and a real patterns redactor, and a **spec-derived MCP fixture** built on
  the OFFICIAL go-sdk (`mcpsdk.NewServer` + `mcpsdk.NewStreamableHTTPHandler` over
  `httptest.NewServer` — the §17.8 shape phase 200 already uses,
  `test/integration/phase200_credential_injection_test.go:103-123`). Legs:
  1. **Restart survival (AC 10):** add through the real wire handler → drop and rebuild the
     runtime-side registry/catalog on the SAME state store → the first run-start reconcile
     re-attaches; the tool is back in the projected planner catalog and in
     `mcp.servers.list`.
  2. **Rollback re-declare (AC 11):** add → remove → reconcile (detaches) → rollback →
     reconcile (re-attaches), through the same leg.
  3. **Identity propagation:** the attached registration carries the reconciling
     `(tenant, agent)` owner; the emitted events carry the run quadruple; a cross-tenant
     run's sweep attaches nothing of the first tenant's and detaches nothing of it either.
  4. **Failure mode A:** the fixture server is stopped — the re-attach fails, the event
     fires with a scrubbed reason, and **the run still completes**.
  5. **Failure mode B:** the fixture is up but the stdio allowlist has been tightened (or
     the injection opt-in flipped off) — the re-attach is refused with the typed sentinel,
     nothing is registered, and the run still completes.
  6. **N=16 concurrent cross-owner stress** through the real seam.
- **Conformance:** N/A — no persistence interface gains a method; the MCP registry is a
  single process-local concrete.
- **Concurrency / leak:** `TestMCPConnectionAttacher_Reattach_ConcurrentOwners` — N=128 per
  owner, two owners, ONE shared `*MCPConnectionAttacher` and ONE shared `*mcp.Registry`,
  under `-race`. Asserts: exactly one attach lands per `(owner, name)` (no double-attach,
  no transport churn), no cross-owner bleed, no cancellation cross-talk (cancelling owner
  A's ctx leaves B's attach intact), and `runtime.NumGoroutine()` back to baseline after
  the attacher's `Close` drains the closer chain (`mcp_attacher.go:256-268`).
- **Wave-end E2E:** `test/integration/wave_v124_test.go` is claimed by THIS plan, so it
  lives inside §2's authority chain rather than only in the coordination file (the
  gate-0 wave-level defect). It composes the wave's surfaces end to end with real drivers,
  identity propagation, ≥1 failure mode and an N≥10 concurrency stress. If another Stage-2
  phase merges after this one, that phase REBASES the file rather than re-creating it.

## Smoke script additions

`scripts/smoke/phase-216.sh` (`# PREFLIGHT_REQUIRES: live-server`) — the skeleton's
`skip` is replaced:

- **Static guards (each individually mutation-verified):**
  1. `projection.ConnectionReattacher` is declared and `ReconcileConnections` takes it.
  2. The attach pass exists and runs AFTER the detach pass in the same function.
  3. `runloop.go` wires a reattacher (the counterpart of today's `grep -c Attach` → 0).
  4. `Reattach` re-applies the stdio allowlist (the `ErrReattachStdioNotAllowed` reference
     is present at the gate, not merely declared).
  5. `Reattach` honours the injection kill-switch (`ErrReattachInjectionDisabled`).
  6. `Reattach` takes a bounded ctx (a `context.WithTimeout` inside the attach path).
  7. **Credential-neutrality guard:** the run-start attach path references none of
     `provider.Token` / `InitiateFlow` / `CompleteFlow` / `TokenStore` — a grep-level
     assertion that the leg cannot have grown a token step, backed by the AC-15 test.
  8. **Report guard:** every class in AC 16's closed set has an emit site — counted, so
     deleting ONE still fails.
  9. Both new canonical event types appear in `events.go`'s `init()` registration list AND
     in the docs generator's join table — counted, so deleting ONE still fails.
  10. `wire-manifest.gen.json` and `docs/site/protocol/events.md` carry both event names
      (proving the generators were re-run, D-223 / D-209).
  11. `ProtocolVersion` is unchanged and no new method string was added.
- **Live:** `mcp.servers.list` answers 2xx with a `.servers` body against the booted dev
  server (the read path is untouched by this phase, and a regression here would mean the
  attach pass corrupted the registry). The route probe uses an EMPTY body and classifies on
  the typed `code`, never the bare status — the phase-211 lesson: a verb that answers a
  genuine miss with `404 not_found` will otherwise convert a real answer into a SKIP
  (§4.2 item 5). The live block degrades to a SKIP on its own rather than exiting, so the
  guard legs still run standalone.
- **Guard tests:** four `go test -race -run` legs — the projection attach pass, the
  attacher's gates + kill-switch, the concurrency/idempotency run, and the integration
  seam. Each FAILS on a genuine failure and SKIPs only when the filter matches no tests.
- Target: **OK ≥ 10, SKIP 0, FAIL 0** against the live preflight build.

## Coverage target

Targets are set at or above the CURRENT MEASURED figure so no target sanctions a
regression (the gate-0 finding against phase 212's draft). Measured with
`go test -cover` on `plan/v124-wave`:

- `internal/runtime/agentcfg/projection`: **81%** — measured 81.0%. The attach pass is new
  branch-heavy code in this package; it must not drag the figure down.
- `internal/runtime/serve`: **84%** — measured 84.4%. Most of the new code lands here
  (`mcp_attacher.go`, `runloop.go`), so this is the load-bearing target.
- `internal/tools/drivers/mcp`: **85%** — measured 85.7%. Reused unchanged; no regression.
- `internal/agentcfg`: **78%** — measured 78.2%. The change is two constants plus godoc
  (no statements beyond the `init()` list entries), so the figure cannot move materially.
- `internal/runtime/agentcfg/protocol`: **85%** — measured 85.3%. Untouched by this phase;
  stated so a reviewer can confirm it did not move.

## Dependencies

Determined by grep, not inherited. Each edge names the shipped artifact this phase calls
or extends.

- **156 (D-287)** — `projection.ReconcileConnections` itself (`projection.go:167`), the
  `ConnectionDetacher` seam (`:66-85`), `Registry.Deregister` / `RuntimeAddedSources`, and
  the deferral clause this phase ends. The hardest edge.
- **167 (D-301)** — the `(tenant, agent)` owner tag and the owner-scoped reconcile VIEW
  (`registry.go:616-630`, `mcp_detacher.go:107-120`). The attach pass runs under exactly
  that owner; without it the leg would be process-global and could replace another owner's
  registration.
- **169 (D-303)** — `auth.ProviderSet` and `ReconcileOAuthProviders` (`projection.go:301-364`).
  Two loads: it is the **bidirectional-reconcile precedent** this leg copies, and it is the
  **ordering prerequisite** (a connection binding an owner-installed provider only resolves
  after the provider leg has installed it — AC 2).
- **168 (D-302)** — `ReconcileDiscoveryOrigins` (`projection.go:245`). A freshly attached
  connection must receive its allowance in the same run start; the pass order is asserted.
- **198 (D-339)** — the live-layer idempotent same-name replace (`attach.go:234-279`) the
  leg REUSES rather than reinvents (§13), and whose "no forced-reconcile verb" ruling this
  phase honours.
- **203 (D-346)** — the wire-carried `injection` mapping and its fail-closed
  `tools.allow_wire_injection` opt-in (`config.go:1048-1068`). Without this edge the
  kill-switch in AC 4.2 has nothing to switch off.
- **206 (D-350)** — `validateConnectionsSection` + `gateStdioConnectionCommands`
  (`addconnection.go:421-469`), which hold BOTH revision doors to one shape authority so
  the spine is a trustworthy input; its godoc names *"any future attach-from-revision leg"*
  — this one. Also `ownedEntry` / the owner-scoped live write.
- **211 (D-355)** — `Registry.Deregister(ctx, name, owner)` and
  `ConnectionDetacher.Detach(ctx, source, owner)` (the owner threading the attach pass
  pairs with), and `Registry.OwnerOf` as the same-name comparison AC 8 inherits.

- **142 (D-271)** — the `tokenexchange` brokered driver and, decisively, its ruling that a
  source is EITHER interactive or brokered with no dual path. It is a dependency in the
  read-and-honour sense: AC 15's two-family table exists because of it.

**Explicitly NOT dependencies: 92m and 92n.** Both are `Pending` (`docs/plans/README.md`),
and the resume-completes-attach continuation they would build is recorded as unimplemented
in shipped godoc (`addconnection.go:132-136`). This phase must be correct with them parked
and must not become correct only once they land — hence the terminal (never parked)
classification of an attach-time `auth_required`.

**Explicitly NOT a dependency: 215.** The gate-0 review (two independent reviewers
agreeing) found the previous draft's dep on 215 false, and the rescope removes even the
indirect case: this leg reconciles `d.agentConfigID` — the agent the run already resolves
against (`runloop.go:772`) — and never needs a caller-named agent. **NOT 214 either:** the
byte-eligibility question gate-0 raised between 214 and 216 is DISSOLVED, not deferred —
it existed only because the old scope handed connection authorship to end users. This
phase adds no descriptor field and no new authorship principal, so a 214 byte-eligible
connection is exactly as (in)eligible after this phase as before it.

## Risks / open questions

- **The honest claim is narrower than "connections survive restarts."** A connection whose
  live transport depended on operator-supplied static `Headers` is NOT restart-survivable,
  because the headers are secret and never persisted (`addconnection.go:88-90`, `:474-476`)
  — and making them persistable would put a credential on a Protocol-readable, diffable,
  rollback-able spine, which is the D-300 shape inverted. The re-attach dials without them
  and fails loud; the admin re-runs `add_mcp_connection`. Per brief 14 §4's wording rule
  this bound is stated in the goals, pinned by AC 4.3 and by a test, and must be stated in
  D-361 and in the operator skill — never softened to the unscoped claim.
- **Re-dial cost is the leg's real operational risk, and the backoff is load-bearing, not
  hygiene.** `Provider.Connect` has no internal timeout (`mcp.go:545-568`) and the reconcile
  is synchronous at run start, so an unresponsive declared server would add its full stall
  to EVERY run's start. AC 6's bounded ctx and AC 9's backoff are the mitigations; if either
  is dropped in implementation the phase has introduced a latency regression on the golden
  path. Flagged here so a reviewer treats them as acceptance criteria, not polish.
- **The leg makes a pre-existing cross-owner name collision visible on a hot path.** Under
  D-301's stated bound, a shared runtime's runtime-added names share one deployment
  namespace and co-tenant admins are trusted. Today a collision surfaces once, at add time.
  After this phase owner A's run start will keep meeting owner B's live registration. AC 8
  handles it (typed, non-retryable, emitted once per descriptor), but the operator-facing
  consequence is new: a Console showing repeated cross-owner conflicts is the honest signal
  that the deployment needs one-runtime-per-tenant. This does not move D-301's trust
  boundary — it reports it.
- **An `auth_required` re-attach has no principal to resume, AND no continuation to hand
  it to.** The add verb parks on the unified pause/resume primitive awaiting an admin
  (`addconnection.go` `ErrAuthRequired`); this leg classifies the same condition as a
  terminal, backoff-eligible failure instead. Two independent reasons: a run-start reconcile
  has no admin request and no consenting principal, and the resume-completes-attach bridge
  is unshipped (`addconnection.go:132-136`, issue #375; phases 92m / 92n both `Pending`).
  Building a pause here would be a second pause shape (§13), not a use of the existing one.
- **The token-durability question, answered — and the premise it corrects.** Harbor ships
  BOTH provider families, and D-271 item 2 forbids a dual path per source: *"A source is
  EITHER interactive (`oauth2`) or brokered (`tokenexchange`), declared in config."* The
  interactive driver DOES run the flow and DOES persist a sealed bearer through the
  StateStore (`provider.go:369` / `:676` → `tokenstore.go:235`, `:253`), so its durability
  is the §9 state driver's — durable on sqlite/postgres, lost on inmem. The brokered driver
  persists nothing (`tokenexchange.go:26-30`, `:555-559`, `:931`). **A statement that Harbor
  "never runs the OAuth flow, never holds or refreshes a token" describes a
  `tokenexchange`-only DEPLOYMENT, not Harbor**, and this plan records that correction so a
  later phase does not design against the narrower claim. It does not change this phase's
  scope: the divergence lands one layer AFTER the attach (see AC 15), because the attach
  resolves a name and never touches a token.
- **What the operator's menu will ultimately want is a JOINED read, and this phase does not
  build it.** After this phase, a connection that could not be re-established is visible as
  a `mcp.connection.reattach_failed` event (live on the bus, historical through the shipped
  `events.list`) — but no single Protocol read joins "declared in the active revision"
  against "live in the registry", so a menu must correlate `agent_config.get` with
  `mcp.servers.list` itself. A per-connection status flag on the server view is the natural
  home (the shipped `needs_allowance` flag on the Console MCP-connections page is the exact
  precedent, D-302), and it is a wire change with its own generated-file cost. Named as the
  successor phase in D-361 rather than half-built here. **The events are the contract this
  phase commits to; the joined read is not.**
- **Event-payload semantics are borrowed, and the borrowing is deliberate.**
  `MCPConnectionLifecyclePayload.Author` is godoc'd *"the identity that drove the add"*
  (`events.go:319-320`); on a reconcile it carries the reconciling RUN's quadruple. The two
  new event types' own godoc states this, and the empty-vs-populated `RunID` is the
  machine-readable discriminator. The alternative — a second payload type differing by one
  doc comment — is the §13 parallel-implementation shape.
- **Why the previously-planned user-scope scope is not merely deferred but rejected.** Three
  findings hold independently of scheduling: the isolation goal claimed a property D-301
  explicitly refuses (the catalog is `byName` with unfiltered `Resolve`, so per-user
  invisibility is achievable only as a projected-VIEW property); the central acceptance
  criterion contradicted its own non-goals (legs 2 and 3 of the reconcile are
  credential-plane and the proposed descriptor forbade the fields they read); and it assumed
  a connection-URL SSRF rule that does not exist (`validateConnection`,
  `addconnection.go:323-395`, performs no URL parse, no scheme check, no host check — the
  private-range refusal lives only on the credential plane). **The operator's actual
  follow-on intent — users enabling ALREADY-CONFIGURED capabilities on their own config at
  config time — does not need connection authorship at all.** Its natural home is the
  shipped `ConfigScopeUser` tool-exposure tier: `userPayloadToDomain` already maps a user's
  payload onto `ToolExposure{PausedServers, DisabledTools}` (`user.go:37-53`), and the
  projection composes it narrow-only (`unionSorted`, `projection.go` — *"a session disable
  set can only ADD to the admin exclusion set"*). Turning that exclusion-based tier into a
  selection surface is a different, smaller phase with no credential plane in it. This
  phase's leg is its prerequisite only in the weak sense that a capability a user selects
  must actually be attached; it is named as the motivating consumer so the primitive is not
  built blind, and is out of scope.
- **§13 primitive-with-consumer is satisfied in-phase, not deferred.** The leg ships with
  two real consumers exercised end to end with tests: restart survival (AC 10) and
  rollback-re-declare (AC 11). Neither waits on a later wave.
- **Unresolved.** Whether the backoff table should survive a process restart. It does not —
  it is in-memory on the attacher — so a crash-loop deployment re-dials a dead server once
  per boot. That is bounded by the boot rate rather than by the run rate, and persisting
  reconcile-attempt state would put runtime scheduling metadata into the state store for the
  first time. Recorded as a follow-up rather than decided here.

## Glossary additions

- **Run-start attach leg** — the pass inside `projection.ReconcileConnections` that attaches
  a runtime-added MCP connection the reconciling owner's active revision DECLARES but the
  live registry does not carry. The symmetric twin of the detach leg; the mechanism behind
  restart survival and rollback-re-declare.
- **Reattach backoff** — the bounded per-`(owner, name)` retry window the run-start attach
  leg holds so a permanently-unreachable declared server is not re-dialled at every run
  start. Reset on success or on a changed descriptor; the first failure per window is
  emitted, the suppressed attempts are counted.

Both land in `docs/glossary.md` in the same PR.

## As-built notes (§4.3 deviations, shipped)

Three deviations from the sketch above, none touching RFC territory. All three are
recorded in D-361 too.

1. **`ConnectionReattacher.Reattach` takes the reconciling run's
   `identity.Quadruple`** — `Reattach(ctx, owner, id, desc) error`, not the
   three-argument shape in "Public API surface". The concrete OWNS event emission
   (it holds the bus, the scrubber, the stable class and the suppression count,
   and emitting from the projection would drag `internal/events` plus emission
   policy into a package that deliberately imports neither), and AC 12's
   machine-readable reconcile-vs-admin-add discriminator is the payload's
   `RunID` — which the ctx at that call site does not carry
   (`runloop.go` builds `taskCtx` with `identity.With`, the triple only).

2. **The concrete lives in `internal/runtime/serve/mcp_reattacher.go`**, beside
   `mcp_detacher.go`, rather than inside `mcp_attacher.go`. The state still hangs
   off the ONE `*MCPConnectionAttacher` and shares its whole-attach lock and
   closer chain, so there is no second attach implementation — only a file
   boundary that makes the leg reviewable. `NewMCPConnectionAttacher` grew a
   VARIADIC option parameter (`WithReattachGates` / `WithReattachTimeout` /
   `WithReattachClock`) rather than four more positional arguments: options are
   applied at construction only (the artifact stays immutable, §5), every option
   defaults FAIL-CLOSED, and the twenty-one existing call sites are unchanged.

3. **The attach pass marks its per-connection errors through a single-chain
   wrapper type**, not a multi-`%w` `fmt.Errorf`. A multi-`%w` value also
   satisfies `interface{ Unwrap() []error }` — the exact shape a caller uses to
   walk an `errors.Join` tree — so the run loop would have descended INTO the
   wrap, seen the cause stripped of its marker, and misclassified an unreachable
   third party as a detach failure, silently skipping the discovery-allowance
   re-apply on every run with an unreachable declared server. Caught by the
   run-loop wiring test, not by review.

**A latent production bug the phase's own test surfaced, fixed in the same PR
(§17.6).** AC 6's bounded per-connection ctx was NOT sufficient on its own.
`TestMCPConnectionAttacher_Reattach_BoundedContext` hung for the full test
timeout against a server that accepts and never answers: the caller's bounded ctx
ends the initialize handshake, but the MCP SDK's session teardown then issues its
own cleanup request on a context this runtime does not own, and the driver's HTTP
client had NO bound at all (`buildHTTPClient` could even return
`http.DefaultClient`). The stall reached the shipped `add_mcp_connection`
request exactly as much as this new leg. Fixed at the driver's shared choke point
with `unownedBoundingTransport` (`internal/tools/drivers/mcp/transport_sse.go`):
a request carrying no deadline of its own, and not a server→client event stream
(identified by `Accept: text/event-stream`), gains a bounded one; a request the
runtime already bounded keeps its own budget, so an operator who raised a slow
tool's `timeout_ms` is never silently pre-empted. `TestBuildHTTPClient_NoHeaders_ReturnsDefault`
pinned the removed shortcut and is rewritten as `TestBuildHTTPClient_AlwaysBounded`.

**Smoke result.** `scripts/smoke/phase-216.sh` — **OK 27, FAIL 0**, one SKIP (the
live `mcp.servers.list` probe). Every static guard was individually
mutation-verified `OK → FAIL`, and six behavioural mutations that COMPILE each
turn a green test red.

**CORRECTION (wave-v1.24 §17.5 checkpoint audit).** The parenthetical above
originally read "which needs a booted dev server's bearer and becomes an OK under
preflight". **That was false, and this file's header claim that weakening any
guard turns an OK into a FAIL "never into a SKIP" was false for this one guard.**
The probe could only ever SKIP, two independent ways: it posted to
`/v1/mcp/servers/list`, a route that does not exist (`mcp.servers.list` dispatches
through `POST /v1/control/{method}`), and it sent the body triple
`harbor-dev/harbor-dev/harbor-dev` while the dev bearer is minted for `dev/dev/dev`
— `harbor-dev` is the token's `kid`, not its identity — so body-identity
reconciliation would have refused it even at the right URL, and the catch-all
`*)` arm laundered that refusal into a SKIP as well. The route, the triple, and
the `*)` arm (now a FAIL, matching phase 214's route-probe convention) are
repaired in the audit PR; only a genuinely unreachable server skips.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] `make protocol-ts-gen-check` and `make protocol-docs-gen-check` pass (two additive
      canonical events → both generators re-run, artifacts committed; never hand-edited)
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target (targets set at measured current)
- [ ] Cross-owner AND cross-tenant isolation tests pass on the attach pass
- [ ] **Concurrent-reuse test passes — N=128 per owner, two owners, one shared
      `*MCPConnectionAttacher` + one shared `*mcp.Registry` under `-race`, asserting no
      data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See
      AGENTS.md §5 + §11 + D-025.
- [ ] **Integration test exists, wires real drivers plus a spec-derived MCP fixture on the
      official go-sdk (§17.8), asserts identity propagation, covers ≥2 failure modes, runs
      under `-race`.** See AGENTS.md §17.
- [ ] `test/integration/wave_v124_test.go` lands with this phase (or is rebased by a later
      Stage-2 merge), and is named in a phase plan rather than only in the coordination file
- [ ] Every smoke guard is mutation-verified to turn an `OK` into a `FAIL`, never a SKIP
- [ ] Glossary updated with both new terms
- [ ] **Credential-neutrality is asserted NEGATIVELY (AC 15): a re-attach with no
      credential available succeeds, no token API is called during the attach, and the
      shortfall surfaces on the first tool call through the shipped path**
- [ ] D-361 filed, recording: the end of D-287's attach-leg deferral (and that no other
      D-287 call moves), the closure of D-287's first accepted window, the credential-plane
      neutrality WITH the two-driver durability table and the correction to the
      "Harbor never holds a token" premise, the injection kill-switch, the non-retryable
      classification of the cross-owner name conflict, the closed reported-failure class
      set, the header bound stated unscoped-claim-free, and the joined declared-vs-live
      read named as the successor phase
- [ ] `docs/skills/use-the-harbor-protocol/SKILL.md` (surface: `protocol`) updated in the
      same PR (§18): the connection-lifecycle paragraph gains restart survival AND its
      header bound
- [ ] `docs/plans/README.md` phase row + detail block reflect this scope, not the rejected one
