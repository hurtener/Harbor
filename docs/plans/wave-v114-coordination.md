# Harbor v1.14.0 — The Live-Credential-Plane Wave (phases 166–167) — wave coordination

> Per CLAUDE.md §17.7 wave delivery cadence. This is the coordination artifact
> for the v1.14.0 wave, which opens on **HA-15**, an ask raised by an external
> white-label implementor:
>
> > "A Protocol write (or a live allowance surface) for the MCP OAuth-discovery
> > ALLOWED ORIGINS and the oauth-provider binding."
>
> It sequences the two phases into staged worktree dispatches, prescribes the
> per-phase gates (two adversarial reviews + live verification each), the
> wave-end E2E, and the §17.5 checkpoint audit that gates any subsequent band.
>
> **Mandate:** v1.13 shipped MCP OAuth-requirement discovery (Phase 164 /
> D-297), but a Protocol-driven consumer cannot USE it — the allowance that
> unblocks the discovery chain's authorization-server hop is restart-required
> yaml, and the runtime add path DROPS it entirely, so discovery is INERT for
> every runtime-added connection. And the provider a discovered requirement
> points at cannot be installed over the Protocol at all. Two phases close the
> loop, on the existing agent-config revision spine, without weakening a single
> security boundary.

---

## Version label — v1.14.0 (settled)

- The latest released tag is **v1.13.1** (`0ce76fb2`; the `--with-server`
  scaffold boot fix). The next minor is **v1.14.0**.
- This wave is purely **additive**: three new Protocol methods, additive wire
  types, an additive `ConfigPayload` section, an additive config key. No
  breaking change, no `ProtocolVersion` bump (RFC §5.3 — that is an RFC
  change). The product release version moves v1.13.x → **v1.14.0**.

---

## 1. Executive summary

Harbor's credential plane is honest but frozen. `tokenexchange` (D-271) pulls a
brokered token at call time and never holds one; the credential-source seam
(D-285) resolves the broker credential from env or a coordinator; the southbound
binding (D-278) names a provider without naming a secret; and discovery (D-297)
reports a server's advertised OAuth requirement without following it. Every one
of those is right. But **all of them are boot-declared**, and Harbor's own
control plane (the agent-config revision spine, D-240/D-283/D-287) can add an MCP
connection at runtime that none of them can serve.

Concretely, three gaps — all verified against the tree:

1. **The allowance is not revisionable.** `agentcfg.MCPConnectionDescriptor`
   (`internal/agentcfg/agentcfg.go:220-243`) carries `OAuthProvider` but NOT
   `OAuthDiscoveryAllowedOrigins`, so the allowance cannot be versioned,
   diffed, or rolled back.
2. **A SHIPPED BUG, not new work.** `MCPConnectionAttacher.Attach`
   (`internal/runtime/serve/mcp_attacher.go:79-87`) builds a
   `config.MCPServerConfig` and OMITS `OAuthDiscoveryAllowedOrigins`. The rest
   of the chain is intact (`mcpdrv.Attach` copies it at
   `internal/tools/drivers/mcp/attach.go:189` → `Registry.Register` snapshots it
   at `registry.go:363` → `OAuthDiscoveryTarget` returns it at `:647-660` →
   `internal/mcpconsole` feeds it to the walker), so the one dropped field makes
   Phase 164's discovery permanently `needs_allowance` for every runtime-added
   connection. **Fixed in-band per §17.6** ("fix what the test finds — no matter
   where the bug lives"), with a regression test that fails without the carry.
3. **No live mutator.** `mcpdrv.Registry`'s `serverEntry.oauthAllowedOrigins` is
   write-once at `Register` (`registry.go:363`, "read-only thereafter") — there
   is no setter, so even a revisioned allowance could not reach a live
   connection.

Plus the provider half: the connection→provider BINDING is already
Protocol-writable end to end (the descriptor and `AttachRequest` both carry
`oauth_provider`), but the provider it names must exist, and the provider LIST
is a boot-built `map[string]auth.OAuthProvider` handed by value into the attach
path — so a Protocol client can bind to a provider it has no way to install.

- **166 (Stage 1)** closes gaps 1/2/3 and ships the D-062 Console consumer.
- **167 (Stage 2)** ships the non-secret provider-descriptor install (the §4.4
  provider-registry seam) and stops the Console silently dropping the binding.

---

## 2. Verified facts the design rests on (live tree, `0ce76fb2`)

Confirmed against the checkout; load-bearing for the plans. Agents re-verify in
their worktree; they do NOT re-derive the design.

- `config.MCPServerConfig` — `internal/config/config.go:1253-1309`;
  `OAuthProvider` at `:1286`, `OAuthDiscoveryAllowedOrigins` at `:1308`. Both
  documented restart-required today.
- **Origin validation already exists:** `validateDiscoveryOrigin`
  (`internal/config/validate.go:2276-2301`) — https-only, host required, no
  path/query/fragment, IP-literal rejected. The write path REUSES it (exported;
  the boot validator becomes its caller). It is NOT re-implemented.
- `config.ToolOAuthProviderConfig` (`config.go:1086-1109`) — `ClientIDEnv` /
  `ClientSecretEnv` name ENV VARS, not values. `ToolOAuthRemoteConfig`
  (`:1134-1154`) — `AuthTokenEnv` likewise. **This is why the Protocol-writable
  descriptor cannot carry any of the three** (hard constraint 1).
- Agent-config revision spine: `internal/agentcfg/agentcfg.go`
  (`MCPConnectionDescriptor` `:220-243`, `ConfigPayload` `:329`, `Registry`
  `:405`); driver `internal/agentcfg/drivers/statestore/statestore.go`
  (`SetRevision` `:216`, `Rollback` `:365`, `Diff` `:400`).
- Method registration is a SIX-place ritual:
  `internal/protocol/methods/methods.go` (the const, plus `canonicalMethods`
  `:963-975`, `canonicalAgentConfigMethods` `:1227-1250`, and
  `canonicalAgentConfigAdminMethods` `:1297-1306`),
  `internal/protocol/singlesource/singlesource.go:145`,
  `internal/protocol/conformance/conformance.go:766`,
  `cmd/harbor-gen-protocol-docs/methods.go:505` — then `make protocol-ts-gen` +
  `make protocol-docs-gen` (D-223 / D-209, THREE lockstep gates).
- **Admin gating is inherited, not written.**
  `internal/protocol/transports/stream/agentconfig_handler.go:133-155` — the
  `default:` arm requires `auth.ScopeAdmin`. A new route is admin-gated FOR FREE
  by simply NOT being added to the session-safe (`:224-230`) or user (`:238-244`)
  exception maps. Reuse `decode` (`:643`), `assertIdentity` (`:655`),
  `writeServiceError` (`:665`).
- Service seams: `internal/runtime/agentcfg/protocol/` — `lockAgent`
  (`service.go:241`), `identityFromScope` (`:683`), `AttachRequest`
  (`addconnection.go:71-93`), `recordConnectionRevision` (`:308-330`).
- **The mechanical gate a new `ConfigPayload` section WILL trip:**
  `internal/runtime/agentcfg/protocol/rebuild_completeness_test.go` reflection-
  walks every section; a setter that fails to carry a new field forward fails
  automatically (D-283). Plan to satisfy it, not to work around it.
- Effect at runtime: the run-start projection
  (`internal/runtime/agentcfg/projection/projection.go`) + `ReconcileConnections`
  (`:141`, the DETACH seam, D-287); the ADD leg is an immediate live attach
  (`internal/runtime/serve/mcp_attacher.go:78-116`).
- Discovery consumer: `internal/tools/auth/discovery.go` —
  `DiscoveryInput.AllowedOrigins` (`:167`), per-call normalisation in `Discover`
  (`:337`), `validateHop` (`:427-462`). **The SSRF dial guard
  (`net.Dialer.Control`) is installed at construction (`:260-281`) but RUNS
  PER-DIAL, POST-DNS-RESOLUTION — it cannot be widened by a runtime-written
  origin.** The `Discoverer` itself needs NO change. This is the fact hard
  constraint 2 rests on.
- Console: `/mcp-connections` →
  `web/console/src/lib/components/mcp-connections/McpDetailRail.svelte` (the
  inert "Discovered OAuth requirement" section `:296-344`, incl. the
  `needs_allowance` status list `:336-342`). The connection WRITE affordance
  lives on a DIFFERENT page: `/agent-config` →
  `web/console/src/lib/components/agentconfig/AddConnectionCard.svelte` +
  `web/console/src/lib/agentconfig/state.svelte.ts:901-942`.
- **`removeMcpConnection` exists in the TS client
  (`web/console/src/lib/protocol/client.ts:1185-1196`) with NO Svelte caller**
  (grep-verified). Phase 156 (D-287) shipped the verb; nothing drives it. Phase
  166 gives it its first caller on the connections editor it builds — an honest
  consumer for an already-shipped verb, on the surface where it belongs.

### Corrections to the pre-dispatch recon

Two, both minor and both folded into the plans:

1. **`Registry.SetRawHTMLTrust` does NOT emit an audit event.** The setter
   (`internal/tools/drivers/mcp/registry.go:834-847`) only mutates under the
   lock and returns the prior value. The **audit-emit-and-fail-closed posture
   lives at the SURFACE handler** — `handleSetRawHTMLTrust`
   (`internal/protocol/mcp.go:843-872`): "Emit the audit event BEFORE returning.
   A failed emit fails the call closed." Phase 166 copies the posture at the
   correct layer: a mutex-guarded registry setter + the fail-closed audit emit at
   the Protocol surface. (Copying it into the registry would have put an event
   bus in a driver's data structure.)
2. **Phase 92k (the `auth.Provider` runtime config-registration seam) is
   PENDING, not shipped** (`docs/plans/README.md:187`). Phase 167 therefore
   cannot assume a runtime provider seam exists — it BUILDS one (for the
   provider LIST). The coordinator's "167 is smaller than it looks" framing is
   right about the BINDING (a Console fix + a round-trip pin) and wrong about the
   INSTALL (a real §4.4 seam replacing a by-value boot map at the attach path).
   167 is sized **L**, and its plan carries the 92k sibling reconciliation so an
   unparked 92k reuses the registry rather than growing a second one.

---

## 3. Phases

Decision numbers are **pre-assigned** (D-300 for 166, D-301 for 167) so parallel
worktree agents never collide in `docs/decisions.md`.

| Phase | Title | Decision | Stage | Size |
|-------|-------|----------|-------|------|
| 166 | Live MCP OAuth discovery-allowance write | D-300 | 1 | M/L |
| 167 | Protocol-installed OAuth provider (non-secret broker-pull shape) + connection→provider binding | D-301 | 2 | L |

### Stage 1 — the allowance (166) · D-300

**166 — Live MCP OAuth discovery-allowance write (internal/agentcfg +
internal/runtime/agentcfg + internal/tools/drivers/mcp + internal/config +
internal/protocol + web/console, M/L).**

Closes gaps 1/2/3. `agentcfg.MCPConnectionDescriptor` gains
`OAuthDiscoveryAllowedOrigins` (non-secret ⇒ revisionable / diffable /
rollback-able for free). **The shipped attacher field-drop is fixed IN-BAND
(§17.6)** with a regression test that fails without the carry. ONE narrow admin
verb — `agent_config.set_mcp_discovery_origins` `{agent_id, name,
allowed_origins[]}`, FULL REPLACE (the only semantic that diffs and rolls back
cleanly) — writes the revision under `lockAgent` (siblings carried forward,
D-283) AND applies it live via a new `Registry.SetOAuthDiscoveryOrigins` mutator
modelled on `SetRawHTMLTrust` (mutex-guarded, identity-mandatory, returns prior).
**Revoke is live and symmetric** (the D-287 lesson): dropping an origin makes the
next discovery refuse that hop AND prunes the already-recorded
`oauth_requirement`'s authorization-server entries fetched from the revoked
origin. No general `update_mcp_connection` patch verb — `url`/`command`/
`transport`/`oauth_provider` are consumed at ATTACH time, so patching them
without a re-attach is a silent half-write; the allowance is the ONE field
re-read per discovery call, which is exactly why it earns a narrow verb.
**Console (D-062), page split resolved deliberately:** the write is single-homed
on `/agent-config` (where revisions/diff/rollback already render — a revisioned
write without them beside it is dishonest), the connections card becomes a real
editor (grant/revoke + the first Svelte caller for Phase 156's
`remove_mcp_connection`), and `/mcp-connections`'s `needs_allowance` status gains
a DEEP-LINK — never a second write form (§13). A boot-declared connection renders
yaml-edit copy and NO link.

Gate: `scripts/smoke/phase-166.sh` (`OK ≥ 3`) — method present, unknown-name
loud error, non-admin `CodeScopeMismatch`, malformed-origin
`CodeInvalidRequest`, manifest presence; the integration test
(`test/integration/phase166_discovery_allowance_test.go`) with the gap-2
regression discriminator + the revoke-prune live leg + a cross-tenant refusal
under `-race`; the D-025 N≥100 registry stress; **the named security tests**
(`TestDiscovery_RuntimeGrantedOrigin_StillRefusesPrivateDial`,
`TestSetMCPDiscoveryOrigins_RejectsNonAdminScope`,
`TestSetMCPDiscoveryOrigins_FailsClosedOnAuditEmitFailure`,
`TestSetMCPDiscoveryOrigins_RevokePrunesRecordedRequirement`); full D-223/D-209
regen.

**Decision D-300:** the MCP OAuth discovery allowance is revisioned agent-config
state with a live write path — admin-only, server-derived authority, the shared
origin validator, and symmetric revoke — never an SSRF widening (the post-DNS
dial guard still refuses a granted origin that resolves private).

### Stage 2 — the provider (167) · Dep 166 · D-301

**167 — Protocol-installed OAuth provider + the connection→provider binding
(internal/config + internal/tools/auth + internal/tools/drivers/mcp +
internal/agentcfg + internal/runtime/agentcfg + internal/protocol + web/console,
L).**

The Protocol-writable provider descriptor is the NON-SECRET broker-pull shape
ONLY — `{name, driver: "tokenexchange", credential_source: "remote",
credential_broker, token_url, auth_url?, scopes[]}`. **A write carrying
`client_id_env`, `client_secret_env`, or `remote.auth_token_env` is REJECTED with
a typed loud error** (hard constraint 1): those name ENV VARS OF THE RUNTIME
PROCESS, and paired with a caller-supplied `token_url` they are an env-var
exfiltration primitive. The process secret is reached by NAME through a new
boot-declared, config/file-only `tools.oauth_credential_brokers[]` list (the
existing `ToolOAuthRemoteConfig` shape + a `name`) — the same name-indirection
`mcp.servers[].oauth_provider` already uses; the inline
`oauth_providers[].remote` block stays valid (backward-compatible). Env-named
local-secret providers and the interactive `oauth2` driver stay config-only.
Backing it: the §4.4 `auth.ProviderRegistry` seam (interface + one
internally-synchronised concrete, D-025), seeded at boot from `BuildProviders`,
consulted by the MCP ATTACH path (`AttachDeps` / `MCPConnectionAttacher`) —
the catalog builder deliberately keeps its boot map (`tools.entries[]` bindings
are restart-required by design; the boundary is a decision, not an omission).
**Install and uninstall ship together** (the D-287 lesson):
`agent_config.remove_oauth_provider` CLOSES the live provider, so a still-bound
connection's next call fails LOUD — never an unauthenticated fallback dial (§13);
rollback past an install runs the SAME uninstall through the run-start reconcile
seam (one mechanism, N triggers). **The binding half is honestly small:** the
wire already carries `oauth_provider` end to end; the Console silently DROPS it
(`state.svelte.ts:912-923`) — 167 stops dropping it (a SELECT populated from the
installed provider list) and pins the round-trip.

Gate: `scripts/smoke/phase-167.sh` (`OK ≥ 3`) — both methods present, a
`client_secret_env`-carrying write REJECTED over the wire (the security invariant
asserted at the smoke layer), unknown-broker loud error, non-admin
`CodeScopeMismatch`, manifest presence; the integration test
(`test/integration/phase167_oauth_provider_install_test.go`) proving install →
bind → **bearer on the wire** → uninstall → loud failure (with a recording
fixture asserting ZERO credential-less requests reach the server) + rollback →
same uninstall + a cross-tenant refusal, under `-race`; the D-025 N≥100
`ProviderRegistry` stress; the sentinel-redaction test (no secret in any
revision / diff / event / response); §17.8 RFC 8693-derived fixtures; full
D-223/D-209 regen.

**Decision D-301:** an OAuth provider descriptor is Protocol-installable ONLY in
the non-secret broker-pull shape; env-var NAMES never cross the wire; installed
providers live in a §4.4 registry the attach path consults; uninstall closes the
provider and fails bound calls LOUD.

---

## 4. Sequencing (§17.7 waves)

**Stage 1 (dispatch first):** 166. Its deps (164, 92f, 156, 152, 92h/92i, 108m,
118) are all shipped, so it is buildable immediately.

**Stage 2 (after Stage 1 merges):** 167. It is a strict dependent, for two
reasons, both structural:

1. **Shared files.** Both phases touch `internal/agentcfg/agentcfg.go`,
   `internal/runtime/agentcfg/protocol/`, `internal/protocol/methods/methods.go`,
   `internal/protocol/types/agentconfig.go`, the stream handler, the wire
   manifest, and `web/console/src/lib/agentconfig/state.svelte.ts`. Run in
   parallel they would conflict in every one of them, and BOTH regenerate
   `wire-manifest.gen.json` — a stale regen is a silent lockstep break the gates
   only catch at CI.
2. **Shared patterns.** 167 reuses the write patterns 166 establishes: the
   audit-fail-closed emit, the boot-declared typed refusal, admin-gating by
   omission from the exception maps, and the `/agent-config` editor surface its
   provider card sits beside.

**No parallel stage in this wave.** Two phases, two stages, serial. That is the
honest shape — inventing a parallel stage here would buy nothing and cost a
manifest conflict.

**Primitive-with-consumer (§13 + D-062).** 166 ships the allowance write WITH its
Console consumer (the `/agent-config` grant/revoke editor + the
`/mcp-connections` deep-link) and gives Phase 156's caller-less
`remove_mcp_connection` verb its first Svelte caller. 167 ships the
`ProviderRegistry` primitive WITH two consumers in the same phase: the install
verbs that exercise it end to end, and the Console binding SELECT that finally
stops dropping `oauth_provider`. Neither primitive lands without a consumer that
exercises it end-to-end with a test.

**Wave-end:** each phase bundles its own real-driver integration test (§17.3:
real drivers on the seam, identity propagation, ≥1 failure mode, `-race`; §17.8:
spec-derived fixtures). 167's integration test IS the wave-end E2E — it composes
the whole wave's surface: a runtime-installed provider, a runtime-added
connection bound to it, a runtime-granted discovery allowance, a discovered
requirement, a bearer on the wire, and a revoke that fails loud. It is bundled
with 167's PR per §17.7 step 5.

**Wave-end checkpoint audit (§17.5).** Runs AFTER 167 merges and covers BOTH
phases. **Do not scope any subsequent band until the audit PR merges.**

---

## 5. Gates (per-phase, binding)

Each phase clears:

1. **Two adversarial reviews.** A read-only adversarial pass after
   implementation, hunting the specific failure shapes the plan's Risks section
   names — **for 166: the write path's origin smuggling** (non-https that
   reparses, an origin with a path, an IP literal, a rebinding hostname, a
   body-supplied `tenant`/`admin` claim) and **the revoke-prune fidelity** (a
   loose origin match that silently keeps stale requirement data; a revoke
   landing mid-`Discover`); **for 167: the secret-bearing-field rejection**
   (an env-var name smuggled through a nested `remote` block, through an unknown
   extra field a lax decoder tolerates, through a `driver: oauth2` descriptor,
   through a path-traversing `credential_broker` name) and **the uninstall
   posture** (does a bound connection actually fail LOUD, or does it quietly
   dial unauthenticated?). Then a second pass after the first round of fixes
   lands. The implement→adversarial→fix loop is the one validated repeatedly
   across the 114–118 and 159–165 sequences.
2. **Live verification.** **166:** against the real test agent + Console — add an
   MCP connection at runtime, probe it, SEE `needs_allowance` on
   `/mcp-connections`, follow the deep-link, grant the origin on
   `/agent-config`, re-probe, and watch the authorization-server half of the
   chain populate — then REVOKE it and watch the requirement's AS entries
   disappear and the status return to `needs_allowance`, all with no restart.
   That round trip IS the phase. **167:** install a provider over the Protocol
   against a fixture coordinator, bind a new connection to it from the Console
   Add card, and confirm the bearer reaches the southbound server; then remove
   the provider and confirm the bound connection's next call fails LOUD in the
   Console (a typed error, not a silent unauthenticated success). Also attempt a
   `client_secret_env`-carrying install BY HAND and confirm the loud rejection.
3. **The standard gate:** `make drift-audit` + `npx markdownlint-cli2 "**/*.md"`
   repo-wide + `make check-mirror` (no AGENTS/CLAUDE touch) + `make preflight`.
   Coverage ≥ the plan's 85% target on new packages.

---

## 6. §16 placement ritual + dispatch checklist (§17.7 step 3)

Each dispatched worktree agent operates **only inside its own worktree** (`pwd`
first; STOP if a path resolves outside it; NEVER `git merge main`; NEVER leave
conflict markers). Per §16, each agent: reads the master-plan detail block + the
cited RFC sections (§6.4, §6.16, §5.2, §6.15, §7 for both phases) + the informing
briefs (09 / 14 / 11 for both) + the predecessor plans in `Deps` + the §16
workflow; fills every template section of its already-authored plan file (**the
plans exist — the agent IMPLEMENTS against them, it does not re-author the
design**); replaces its `scripts/smoke/phase-NNN.sh` skeleton's `skip` with real
assertions; keeps its pre-assigned `D-NNN` block (D-300 / D-301 — already
authored in this plans PR; the implementation PR updates its **Status** /
**As-built** notes, markdownlint-clean — **blank lines around `---` and around
each `## D-NNN` heading**, the hygiene regression that has broken CI one PR late,
repeatedly); updates any §18 skill / recipe / site surface it touches in the same
PR; and runs the full lockstep regen.

**Wire discipline.** BOTH phases add canonical methods, so both run the SIX-place
registration ritual (§2 above) plus `make protocol-ts-gen` +
`make protocol-docs-gen` with the regenerated `wire-manifest.gen.json` and
`docs/site/protocol/*.md` committed (D-223 / D-209). The manifest is GENERATED —
hand-editing it is rejection-on-sight. Because the stages are serial, there is no
second-merged-rebase concern; 167 rebases onto a main that already carries 166's
manifest and re-runs both generators before its final push.

**Godoc-visible-source discipline (§13 / phase-102).** No `Phase NN` /
`phase-NN`, inline `D-NNN`, `brief NN`, or wave-band references in non-test Go
source under `internal/`, `cmd/`, or `sdk/`. Name the FEATURE, not the number.
Acute here because both phases add fresh godoc-visible surface
(`Registry.SetOAuthDiscoveryOrigins`, `auth.ProviderRegistry`, two service
files) — the drift-audit godoc gate will fail on a stray reference.

**No new top-level directory.** Every file lands under an existing tree
(`internal/tools/auth/`, `internal/agentcfg/`, `internal/runtime/agentcfg/`,
`internal/protocol/`, `web/console/src/lib/`). §3's binding layout is unchanged —
no RFC layout PR needed.

**Security-test discipline.** The named tests in each plan's Test plan are
BINDING, not illustrative. A phase that ships without
`TestDiscovery_RuntimeGrantedOrigin_StillRefusesPrivateDial` (166) or
`TestSetOAuthProvider_RejectsEnvNamedSecretFields` (167) has not shipped its
central invariant, whatever else is green.

---

## 7. The six hard constraints (binding across both phases)

Carried into both plans as explicit security invariants with named tests. They
are not negotiable by the implementor; a deviation is an RFC PR.

1. **No env-var NAMES over the wire.** The Protocol-writable provider descriptor
   MUST NOT accept `client_id_env`, `client_secret_env`, or
   `remote.auth_token_env`. Those name environment variables of the runtime
   process; an `admin` caller who can write an env-var name AND a `token_url`
   has an ENV-VAR EXFILTRATION PRIMITIVE (point the token endpoint at an
   attacker host, name any env var, receive its value in the token request). The
   Protocol-writable shape is the NON-SECRET broker-pull form only. A write
   carrying any of the three is REJECTED with a loud typed error — never
   silently ignored, never stripped-and-accepted. (167;
   `TestSetOAuthProvider_RejectsEnvNamedSecretFields`.)
2. **A runtime-written allowance is still dial-guarded.** The walker's
   `net.Dialer.Control` hook runs POST-DNS-RESOLUTION, so a granted origin whose
   hostname resolves to a private / loopback address is STILL refused.
   Allowance ≠ SSRF bypass. The write path additionally reuses the SHARED
   `validateDiscoveryOrigin` — no second origin parser. (166;
   `TestDiscovery_RuntimeGrantedOrigin_StillRefusesPrivateDial`.)
3. **Revoke must actually take effect on the LIVE connection.** The D-287 lesson:
   v1.11 shipped a detach-only reconcile and ate the asymmetry. A revoked
   allowance must not merely stop being re-read at next boot — it takes effect
   immediately AND prunes the requirement data the withdrawn allowance already
   produced (166). An uninstalled provider CLOSES, so a still-bound connection's
   next call fails LOUD rather than degrading to an unauthenticated dial (167).
   Where a reconcile leg is genuinely out of scope, the plan says so EXPLICITLY
   in the plan, the godoc, and the Console copy — no silent half-write.
4. **Authorization is server-derived and `admin`-gated.** Reuse
   `auth.HasScope(ctx, auth.ScopeAdmin)`
   (`internal/protocol/auth/scopes.go:115`) and `resolveIdentity(r)`
   (`stream.go:439`). NEVER read authority off the request body (D-219). The
   scope set is CLOSED — do not mint a new scope (D-284).
5. **On the agent-config revision spine.** Versioned, diffable, rollback-able,
   under `lockAgent`, carrying every sibling section forward (D-283). Not a
   side-channel, not a shadow store.
6. **A Console consumer in the SAME wave** (D-062 + §13's
   primitive-without-consumer rule). Each phase's consumer ships in its own PR,
   not "later."

---

## 8. Open questions — resolved before dispatch

1. ~~Where does the allowance WRITE affordance live — `/mcp-connections` (where
   the operator sees `needs_allowance`) or `/agent-config` (where connection
   writes live)?~~ **RESOLVED: single-homed on `/agent-config`**, because the
   allowance is a field of the revisioned connection descriptor and a revisioned
   write with no diff/rollback affordance beside it is dishonest.
   `/mcp-connections` gets a DEEP-LINK carrying the connection name + the refused
   origin, never a second write form (§13). D-300.
2. ~~A registry MUTATOR or a full re-register for the live allowance?~~
   **RESOLVED: a mutator** (`Registry.SetOAuthDiscoveryOrigins`, modelled on
   `SetRawHTMLTrust`). The allowance is a pure per-call policy input to the
   walker — read fresh on every `Discover` via `OAuthDiscoveryTarget`, which
   already returns a COPY under `RLock`. Nothing about the live transport depends
   on it. A re-register would tear down a working transport, kill in-flight
   calls, re-run the handshake, and can FAIL — leaving the connection down after
   a WIDENING edit. A mutex-guarded setter is the D-025-correct shape and makes
   revoke's live effect trivial. D-300.
3. ~~Does the allowance write get a general `update_mcp_connection` patch verb?~~
   **RESOLVED: no.** `url` / `command` / `transport` / `oauth_provider` are
   consumed at ATTACH time and held by the live transport; patching them without
   a re-attach is a silent half-write. The allowance is the ONE field re-read per
   discovery call — hence a narrow, purpose-built verb. Changing a binding stays
   remove + re-add, stated in the godoc and the Console copy. D-300 / D-301.
4. ~~How does a Protocol-installed provider get its coordinator bearer without an
   env-var name on the wire?~~ **RESOLVED:** a boot-declared, config/file-only
   `tools.oauth_credential_brokers[]` NAMED list holds the `auth_token_env`; the
   installed provider references it by non-secret NAME — the third instance of the
   name-indirection pattern already used by `mcp.servers[].oauth_provider` and
   `tools.entries[].oauth.provider`. The inline `oauth_providers[].remote` block
   stays valid. D-301.
5. ~~Is the interactive `oauth2` driver Protocol-installable?~~ **RESOLVED: no.**
   It needs an env-named client secret and a browser redirect. Config-only. A
   "trusted admin" carve-out would re-open the exfiltration primitive the phase
   exists to close. D-301.
6. ~~Does the CATALOG builder consult the live provider registry too?~~
   **RESOLVED: no** — `tools.entries[]` OAuth bindings are boot-declared and
   restart-required by design, and the catalog's middleware wrapping is
   boot-ordered (D-292). Only the MCP ATTACH path consults the registry. A
   deliberate boundary, named so a future phase does not "discover" it as a bug.
   D-301.
7. ~~Is `removeMcpConnection`'s missing Svelte caller in scope?~~ **RESOLVED:
   yes, in 166** — the connections card 166 turns into a real editor is exactly
   where a remove affordance belongs, beside the grant/revoke affordance on the
   same revisioned surface. Included because it belongs there, not because it is
   cheap.
8. ~~Any `ProtocolVersion` bump?~~ **RESOLVED: no.** Three additive methods,
   additive wire types, an additive config section — no breaking change. Full
   D-223 / D-209 regen; the version pin is untouched.
