# Harbor v1.14.0 — The Credential-Plane-Hardening Wave (phases 166–169) — wave coordination

> Per CLAUDE.md §17.7 wave delivery cadence. This is the coordination artifact
> for the v1.14.0 wave, which opens on **HA-15**, an ask raised by an external
> white-label implementor:
>
> > "A Protocol write (or a live allowance surface) for the MCP OAuth-discovery
> > ALLOWED ORIGINS and the oauth-provider binding."
>
> **The direction stands; the shape was rebuilt.** The first-cut v1.14 shape
> (two phases guarded by "no env-var NAMES on the wire") drew two independent
> adversarial NO-GO reviews that CONVERGED: that rule is a symptom rule, not the
> invariant. This wave replaces it with the generalized invariant and a
> bottom-up four-phase build.

---

## 0. The central correction (binding)

**"No admin-writable field may determine where a credential is sent."** This
supersedes "no env-var NAMES on the wire." Two proofs the symptom rule did not
close the hole, both against SHIPPED Harbor (92f's `add_mcp_connection` +
D-278's southbound binding), neither an HA-15 regression:

1. **`token_url` stayed on the writable descriptor.**
   `internal/tools/auth/drivers/tokenexchange/tokenexchange.go:582` POSTs the
   broker-resolved `client_id` + `client_secret` (and the identity
   `subject_token`) to exactly that URL. An admin names a LEGITIMATE
   boot-declared broker + an attacker `token_url` → receives the org's real
   OAuth `client_secret`. The named-broker indirection RENAMED the sink; it did
   not remove it.
2. **Even removing every URL from the descriptor is insufficient** — the
   CONNECTION `url` is admin-writable, and the exchanged downstream token is
   injected there as a `Bearer`. `resolveOAuthBinding`
   (`internal/tools/drivers/mcp/attach.go:288-320`) checks transport /
   empty-URL / static-header conflict / provider existence and places NO
   constraint on the host; `add_mcp_connection` validation only checks "http
   transport requires a url", never WHICH url. And the provider `name` IS the
   default audience (`tokenexchange.go:214-217`), so the caller picks the
   audience+scopes of the token they exfiltrate.

The invariant names the PROPERTY (a credential sink) instead of one instance, so
it closes the whole class. Every sink-determining value — the token endpoint,
the allowed downstream hosts, the audience, the scope ceiling — is boot-declared.

---

## 1. Executive summary

Harbor's credential plane is honest but had three shipped exfil paths and two
process-global registries that break the multi-tenant framework promise. The
wave is built bottom-up so each phase stands on a hardened base:

- **166 (D-300) — credential-sink hardening.** No new Protocol surface; pure
  security fix of shipped code. Boot-declared downstream-host allow-list
  enforced in `resolveOAuthBinding`; audience/scope ceiling (not derived from a
  caller-chosen name); a hardened, redirect-refusing token-exchange client; and
  the `handleSetRawHTMLTrust` audit-ordering lie fixed (both write phases were
  about to copy it as their audit posture).
- **167 (D-301) — owner-scoped reconcile for runtime-added entries.** Harbor is
  a FRAMEWORK: downstream teams run one runtime per tenant OR one runtime
  serving many. Boot infra stays PROCESS-GLOBAL (D-287 preserved); runtime-ADDED
  connections + Protocol-installed providers carry an OWNER tag `(tenant,agent)`
  used only for revision ownership + reconcile-VIEW scoping (a run-start
  reconcile touches only its own owner's runtime-adds — never boot, never
  another owner). NOT full-triple keying (which two reviews proved breaks the
  common deployment, doesn't isolate, and reverses D-287). No false
  isolation-of-dispatch claim; the two NOTEs are rewritten.
- **168 (D-302) — the discovery-allowance write** (the first-cut 166), now
  landing on the process-global bare-name registry (D-287) + the owner-scoped
  reconcile (167), with the false "shipped bug" defect statement corrected to a
  wiring gap and a run-start allowance-reconcile leg so rollback takes effect
  live.
- **169 (D-303) — provider install + binding** (the first-cut 167, de-scoped),
  writable shape carrying ZERO URLs — the token endpoint and downstream-host set
  are pinned at boot on the named broker.

---

## 2. Version label — v1.14.0 (settled)

- Latest released tag: **v1.13.1** (`0ce76fb2`). The next minor is **v1.14.0**.
- 166 + 167 are shipped-code fixes (config-schema + internal); 168 + 169 add
  three additive Protocol methods + additive wire/config surfaces. No breaking
  change, no `ProtocolVersion` bump (RFC §5.3). The product release version
  moves v1.13.x → **v1.14.0**. The 166/167 hardening ships IN-BAND (§17.6), not
  as a separate patch release (operator decision 1).

---

## 3. Verified facts the design rests on (live tree, `0ce76fb2`)

Confirmed against the checkout; load-bearing. Agents re-verify in their
worktree; they do NOT re-derive the design.

- **The three exfil sinks** — `tokenexchange.go:582` (POST to `token_url`),
  `tokenexchange.go:214-217` (audience = provider name), `attach.go:288-320`
  (`resolveOAuthBinding` constrains no host), `tokenexchange.go:242-245` (bare
  `&http.Client{Timeout:30s}`, follows redirects). The in-repo hardening
  precedents: `discovery.go:260-281` (post-DNS `net.Dialer.Control`) and
  `credsource/drivers/remote/remote.go:127-134` (refuses every redirect because
  it carries a bearer).
- **The audit-ordering lie:** `handleSetRawHTMLTrust`
  (`internal/protocol/mcp.go:843-875`) godoc claims fail-closed, but it applies
  the mutation THEN emits; on emit failure its own error reads "trust toggle
  applied but audit emit failed." (Recon correction: `Registry.SetRawHTMLTrust`
  itself, `registry.go:834-847`, emits NOTHING — the audit posture lives at the
  surface handler, and it is the handler's ordering that is wrong.)
- **The two process-global registries:** `mcpdrv.Registry.servers`
  (`registry.go:273-275`, blind overwrite `:353`, pointer-returning read
  `:605`); the by-value provider map (`attach.go:288`,
  `catalog.go:168-173`). `lockAgent` shards on `(scope, tenant, agent)`
  (`service.go:lockOwner`) — different tenants take different locks and then
  clobber the same name-keyed entry. The two conceded NOTEs:
  `projection.go:125-132`, `mcp_detacher.go:77-87`.
- **The discovery-allowance wiring gap (recon CORRECTION):**
  `AttachRequest` (`addconnection.go:71-93`), `agentcfg.MCPConnectionDescriptor`
  (`agentcfg.go:220-243`), and the wire descriptor
  (`types/agentconfig.go`) ALL lack an allowance field. Nothing DROPS it —
  nothing CARRIES it. The earlier "Attach drops the field" statement is false;
  the mechanism is a §17.1 cross-package wiring gap (164 shipped the walker, 92f
  the add path, neither joined them). The "regression test fails against the
  pre-fix attacher" discriminator was unachievable.
- **The origin validator already exists:** `validateDiscoveryOrigin`
  (`internal/config/validate.go:2276-2301`) — reused (exported) by 168, not
  re-implemented.
- **The walker's SSRF guard runs post-DNS** (`discovery.go:260-281`) — a
  runtime-written origin cannot widen it (allowance ≠ SSRF bypass).
- **The binding is already Protocol-writable end to end** (`oauth_provider` on
  the descriptor, `AttachRequest`, and `types/agentconfig.go`); the Console
  DROPS it (`state.svelte.ts:912-923`).
- **`removeMcpConnection`** exists in the TS client
  (`client.ts:1185-1196`) with NO Svelte caller (grep-verified) — 168 gives it
  its first caller.
- **Method registration is a six-place ritual** (`methods.go` const + the three
  canonical sets, incl. the prose counts at `:1219` / `:1290`;
  `singlesource.go`; `conformance.go`;
  `cmd/harbor-gen-protocol-docs/methods.go`) + `make protocol-ts-gen` +
  `make protocol-docs-gen` (D-223 / D-209).
- **Admin gating is inherited, not written**
  (`agentconfig_handler.go:133-155`; a route is admin-gated by NOT joining the
  session-safe `:224-230` / user `:238-244` exception maps).
- **The rebuild-completeness guard** (`rebuild_completeness_test.go`) fails
  automatically on a new `ConfigPayload` section a setter forgets (D-283).
- **Phase 92k is PENDING, not shipped** (`docs/plans/README.md:187`) — 169
  therefore BUILDS the provider set; it does not assume one exists.
- **`auth.ProviderSet` naming:** `internal/tools/auth/registry.go` ALREADY
  exists as the OAuth DRIVER registry; the new provider-INSTANCE set lands in a
  NEW `providers.go` and is NOT a §4.4 driver seam (it holds instances, no
  `drivers/prod` registration).

---

## 4. Phases

Decision numbers are **pre-assigned** (D-300..D-303) so parallel worktree agents
never collide in `docs/decisions.md`.

| Phase | Title | Decision | Stage | Size |
|-------|-------|----------|-------|------|
| 166 | Credential-sink hardening (shipped-code) | D-300 | 1 | M |
| 167 | Owner-scoped reconcile for runtime-added entries | D-301 | 2 | M |
| 168 | Live MCP OAuth discovery-allowance write | D-302 | 3 | M/L |
| 169 | Protocol-installed OAuth provider (zero-URL) + binding | D-303 | 4 | L |

### Stage 1 — the base (166) · D-300

No new Protocol surface. Boot-declared `AllowedDownstreamHosts` allow-list
enforced in `resolveOAuthBinding` (fail-closed on empty for a bindable provider;
covers the `oauth2` binding too); a boot audience/scope ceiling; the
token-exchange client hardened (post-DNS dial guard, `Proxy: nil`,
refuse-redirects — the `credsource/remote` precedent); the
`handleSetRawHTMLTrust` audit-ordering fix + a shared fail-closed helper 168/169
reuse. In-band per §17.6. Gate: `scripts/smoke/phase-166.sh` (`unit-tests`,
`OK ≥ 2`) + the credential-sink integration test (a redirecting broker fixture
never receives a re-POSTed `client_secret`; an unlisted downstream host is
refused at attach) under `-race`.

### Stage 2 — the safe base for writes (167) · Dep 166 · D-301

Boot infra stays process-global (D-287 preserved — do NOT key the boot registry
or the bare-name catalog). Runtime-ADDED connections + Protocol-installed
providers carry an OWNER tag `(tenant,agent)` used only for revision ownership +
RECONCILE-VIEW scoping: a run-start reconcile touches only its own owner's
runtime-adds. The two NOTEs are REWRITTEN (not deleted) to describe the
deliberate process-global boot behaviour + the owner-scoped view. The wave
claims NO hard cross-tenant isolation of runtime-added DISPATCH (a shared
runtime trusts co-tenant admins; a name collision fails loud; hard isolation =
one-runtime-per-tenant) — no false safety property (the prior draft's FAIL-A).
Single-tenant behaviour-identical. No new Protocol surface. Gate:
`scripts/smoke/phase-167.sh` (`unit-tests`, `OK ≥ 2`, incl. the corrected
NOTE-mentions-owner-scoped static trip-wire) + the owner-scoped-reconcile +
boot-visibility integration test under `-race`.

### Stage 3 — the allowance write (168) · Dep 167 · D-302

The first-cut 166, landing on the process-global bare-name registry (D-287) +
the owner-scoped reconcile (167). The descriptor gains the non-secret allowance;
the wiring gap is closed end to end; ONE admin verb
`agent_config.set_mcp_discovery_origins` (FULL REPLACE) writes the revision AND
applies live via a bare-name mutator; revoke is live+symmetric (fresh
requirement, pointer swap under the lock — WARN 10); rollback takes effect live
via an OWNER-scoped run-start reconcile leg doing a FULL idempotent re-prune
(FAIL 7 + WARN 11); allowance ≠ SSRF bypass; audit via 166's
helper; Console consumer (single-homed write on `/agent-config`, deep-link from
`/mcp-connections` resolving the connection→agent mapping — item 12; first caller
for `remove_mcp_connection`). Gate: `scripts/smoke/phase-168.sh` (`live-server`,
`OK ≥ 3`) + the integration test (the wiring round-trip, revoke-prune,
rollback-live-revoke, cross-tenant isolation) under `-race`; full D-223/D-209
regen.

### Stage 4 — the provider install (169) · Dep 166 + 167 + 168 · D-303

The first-cut 167, de-scoped. Writable descriptor carries ZERO URLs
(`{name, credential_broker, scopes?}`); the sink is pinned on the named broker
(166). `auth.ProviderSet` (new `providers.go`, bare-name resolution +
owner-tagged reconcile, NOT the driver registry, NOT a §4.4 seam). Install +
uninstall together; uninstall CLOSES the provider (bound call fails LOUD);
rollback = same uninstall; owner-safe via 167's owner-scoped reconcile. The binding half stops the Console drop. Gate:
`scripts/smoke/phase-169.sh` (`live-server`, `OK ≥ 3`, incl. a
`token_url`/`client_secret_env`-rejection probe over the wire) + the integration
test (install → bind → bearer on the wire → uninstall → loud failure → rollback;
cross-tenant safety; sentinel-redaction) under `-race`; full D-223/D-209 regen.
**169's integration test IS the wave-end E2E** — it composes the whole wave's
surface end to end.

---

## 5. Sequencing (§17.7 waves)

**Strictly serial, four stages, one phase each.** The dependency chain is
structural, not a scheduling preference:

- **166 first** — the hardened token-exchange edge + the audit-fail-closed
  helper are the base 168/169's audit ACs and 169's sink-pinning rely on.
- **167 second** — the write phases' RECONCILE legs must land on the
  owner-scoped reconcile, or they amplify the process-global reconcile into a
  cross-owner auth-outage primitive (a tenant-B run reconciling away tenant-A's
  allowance / provider). `lockAgent` gives zero cross-tenant protection (FAIL 5)
  — but the fix is owner-scoping the reconcile VIEW, NOT keying the registry
  (D-287 preserved). 168's rollback re-prune and 169's uninstall/rollback are
  only owner-safe once 167 lands.
- **168 third, 169 fourth** — both touch `agentcfg.go`, the agent-config
  service, `methods.go`, `types/agentconfig.go`, the stream handler, the wire
  manifest, and `state.svelte.ts`; run in parallel they conflict everywhere and
  both regenerate `wire-manifest.gen.json` (a stale regen is a silent lockstep
  break). Serial means 169 rebases onto a main already carrying 168's manifest
  and re-runs both generators before its final push.

**No parallel stage.** Four dependent phases; inventing a parallel stage would
buy nothing and cost a manifest conflict.

**Primitive-with-consumer (§13 + D-062).** 166 ships the allow-list + hardened
client WITH the tests that exercise the exfil paths end to end. 167 ships the
owner-scoped reconcile WITH the boot-visibility + owner-scoped-reconcile test. 168 ships the write WITH its
Console consumer (the `/agent-config` editor + the `/mcp-connections` deep-link)
and gives `remove_mcp_connection` its first Svelte caller. 169 ships the
`ProviderSet` primitive WITH two consumers: the install verbs and the Console
binding SELECT that finally stops dropping `oauth_provider`.

**Wave-end + checkpoint.** 169's integration test is the wave-end E2E (§17.7
step 5). The §17.5 checkpoint audit runs AFTER 169 merges and covers all four
phases. **Do not scope any subsequent band until the audit PR merges.**

---

## 6. Gates (per-phase, binding)

1. **Two adversarial reviews.** Hunt the specific failure shapes each plan's
   Risks section names — for 166 the sink-invariant completeness (did every
   admin-writable sink get pinned? try `token_url`, the connection host, the
   audience, a redirect) and the audit-fail-closed correctness; for 167 an
   unkeyed call slipping through (fail-closed catches it) and the single-tenant
   equivalence; for 168 the write-path origin smuggling, the revoke-prune
   pointer race, and the rollback-live-revoke; for 169 the ZERO-URL structural
   invariant (smuggle a URL/env-var name through an unknown field, a
   `driver: oauth2`, an empty `credential_source`, a path-traversing broker
   name) and the uninstall-fails-loud posture. Then a second pass after the
   first round of fixes.
2. **Live verification.** 166: drive a token-exchange against a fixture broker,
   confirm a binding to an unlisted downstream host is refused at attach and a
   redirecting broker never receives the `client_secret`. 167: boot two tenants,
   add same-named connections + providers, confirm a boot server stays visible
   to every session AND an owner-scoped reconcile never detaches another owner's
   add (the bounded guarantee — NOT hard dispatch isolation) by hand. 168:
   against the real test agent + Console — add a connection, probe, SEE
   `needs_allowance` on `/mcp-connections`, follow the deep-link, grant on
   `/agent-config`, re-probe (AS hop populates), REVOKE (entries disappear,
   status returns), all with no restart. 169: install a provider over the
   Protocol against a fixture coordinator, bind a connection from the Console Add
   card, confirm the bearer reaches the server; remove the provider and confirm
   the bound connection's next call fails LOUD; attempt a `token_url`-carrying
   install by hand and confirm the loud rejection.
3. **The standard gate:** `make drift-audit` (FAIL 0) + `npx markdownlint-cli2
   "**/*.md"` (0 errors) + `make check-mirror` + `make preflight`. Coverage ≥
   85% on new packages.

---

## 7. §16 placement ritual + dispatch checklist (§17.7 step 3)

Each dispatched worktree agent operates **only inside its own worktree** (`pwd`
first; STOP if a path resolves outside it; NEVER `git merge main`; NEVER leave
conflict markers). Per §16, each agent: reads the master-plan detail block + the
cited RFC sections (§6.4, §6.15, §7 for 166; §6.4, §6.9, §6.16, §7 for 167;
§6.4, §6.16, §5.2, §6.15, §7 for 168/169) + the informing briefs (09/03 for 166;
05/03 for 167; 09/14/11 for 168/169) + the predecessor plans in `Deps` + the §16
workflow; fills every template section of its already-authored plan file (**the
plans exist — the agent IMPLEMENTS against them**); replaces its
`scripts/smoke/phase-NNN.sh` skeleton's `skip` with real assertions (matching the
declared `PREFLIGHT_REQUIRES` — `unit-tests` for 166/167, `live-server` for
168/169); keeps its pre-assigned `D-NNN` block (D-300..D-303 — the
implementation PR updates **Status** / **As-built**, markdownlint-clean, **blank
lines around `---` and each `## D-NNN`**); updates the named §18 skills; and runs
the full lockstep regen (168/169).

**Wire discipline.** 166/167 add NO wire surface (a manifest diff is a red
flag). 168/169 add canonical methods — the six-place registration ritual + the
prose counts (NIT 19) + `make protocol-ts-gen` + `make protocol-docs-gen` with
regenerated artifacts committed (D-223 / D-209). Serial staging means 169 rebases
onto 168's manifest and re-runs both generators.

**Godoc-visible-source discipline (§13 / phase-102).** No `Phase NN` /
`phase-NN`, inline `D-NNN`, `brief NN`, or wave-band references in non-test Go
source under `internal/`, `cmd/`, or `sdk/`. Acute for the fresh godoc-visible
surface: `Registry.SetOAuthDiscoveryOrigins`, `auth.ProviderSet`, the new service
files, the keyed registry methods.

**No new top-level directory.** Every file lands under an existing tree. §3's
binding layout is unchanged — no RFC layout PR.

**Security-test discipline.** The named tests in each plan's Test plan are
BINDING, not illustrative — e.g. 166's
`TestResolveOAuthBinding_RefusesUnlistedDownstreamHost` +
`TestTokenExchange_HTTPClient_RefusesRedirect`, 167's
`TestReconcile_OwnerScoped_NeverDetachesBootOrOtherOwner`, 168's
`TestReconcile_RollbackPastGrant_RevokesOriginLive`, 169's
`TestSetOAuthProvider_RejectsSinkAndSecretFields`.

**§18 skills (named, not a grep instruction — WARN 14).**
`docs/skills/define-the-agent-yaml/SKILL.md` (`surface: agent-yaml`) is bound by
166's `allowed_downstream_hosts` + ceiling config keys and 169's named-broker
example; `docs/skills/use-the-harbor-protocol/SKILL.md` (`surface: protocol`) is
bound by 168/169's new methods. There is NO `surface: mcp` skill.

---

## 8. The six hard constraints (binding across the wave)

Carried into the plans as explicit security invariants with named tests. Not
negotiable by the implementor; a deviation is an RFC PR.

1. **No admin-writable field may determine where a credential is sent** (D-300,
   the generalized invariant). Enforced structurally: 166 pins every sink on
   boot-declared config; 169's writable descriptor carries ZERO URLs.
2. **A runtime-written allowance is still dial-guarded** — the walker's
   post-DNS `net.Dialer.Control` refuses a granted private/loopback origin
   (168). Allowance ≠ SSRF bypass.
3. **Revoke / uninstall take effect on the LIVE connection** (the D-287 lesson).
   168: revoke prunes the recorded requirement + rollback revokes live via the
   reconcile leg. 169: uninstall CLOSES the provider so a bound call fails LOUD,
   confined to the owning tenant by 167's keying.
4. **Authorization is server-derived and `admin`-gated** (D-219). Routes inherit
   the handler's `default:` arm by omission; identity from the verified ctx,
   never the body. No new scope (D-284).
5. **On the agent-config revision spine** — versioned, diffable, rollback-able,
   under `lockAgent`, siblings carried forward (D-283). And — new this wave — on
   the OWNER-SCOPED reconcile (167), because `lockAgent` alone gives zero
   cross-tenant protection and the write phases' reconcile legs must be
   owner-scoped (the registry itself stays process-global bare-name — D-287).
6. **A Console consumer in the SAME phase** (D-062 + §13) for 168 and 169.

---

## 9. Punch-list disposition (from the two reviews)

Every FAIL/WARN/NIT is resolved in the plans; NONE is rejected. Summary:

- **FAIL 1/2/3 (the three exfil sinks)** → Phase 166 (D-300).
- **FAIL 4/5/6 (process-global registry over-detach; `lockAgent` zero
  cross-tenant protection; reconcile-uninstall cross-owner outage)** → Phase 167
  (D-301) — via OWNER-scoped reconcile, NOT triple-keying (which two reviews
  rejected: it breaks the `mcpDefault` deployment, doesn't isolate the bare-name
  catalog, and reverses D-287). 168/169 depend on it.
- **FAIL 7 (rollback has no live effect)** → Phase 168's run-start
  allowance-reconcile leg + AC + test.
- **WARN 8 (false "shipped bug drops the field")** → restated as a §17.1 wiring
  gap in D-302, the coordination doc, the master-plan block, and the plan; the
  discriminator re-scoped to a within-phase round-trip guard.
- **WARN 9/20 (`ProviderSet` naming / not a §4.4 seam)** → new `providers.go`,
  named for instances, explicitly not the driver registry and not a driver seam.
- **WARN 10 (revoke-prune pointer race)** → build a fresh requirement, swap under
  the lock; pinned in the `-race` test.
- **WARN 11 (allowance-generation only in Risks)** → DELETED; the mid-walk
  self-heal is bounded by 168's FULL IDEMPOTENT run-start re-prune (re-derives
  each connection's allowance from the current revision, not a delta) — pinned
  by a named test.
- **WARN-C (169 depended on a broker 166 never built)** → 166 now BUILDS the
  boot `tools.oauth_credential_brokers[]` list (name + token_url +
  allowed_downstream_hosts + auth_token_env); 169's zero-URL descriptor
  references it by name. Both plans agree on what exists.
- **WARN-D (the exchanged MCP bearer can egress via a redirect)** → 166 gives
  the MCP bearer client a `CheckRedirect` re-validating the redirect target
  against `AllowedDownstreamHosts` (the `bearerInjectingTransport` re-injects on
  every hop, `transport_sse.go:100-111`, and the client was on the default
  redirect policy). Named 166 AC.
- **FAIL-A (167's false isolation claim) / FAIL-B (167's non-discriminating
  test)** → 167 reframed to owner-scoped reconcile with the bounded guarantee
  stated plainly; the test replaced by
  `TestRegistry_BootServerVisibleToEverySession` +
  `TestReconcile_OwnerScoped_NeverDetachesBootOrOtherOwner`.
- **WARN 12 (deep-link has no agent context)** → resolved (read connection→agent
  from the agent-config registry before rendering; boot-declared/unowned →
  honesty copy + vitest).
- **WARN 13 (92m collision)** → ruled in D-302; the 92k + 92m pointer notes are
  ACTUALLY written in this PR.
- **WARN 14 (skill hygiene named)** → the two skills named in §7.
- **WARN 15 (`auth_url` dead weight) / 16 (empty `credential_source`) / 17
  (field-naming reject)** → 169's ZERO-URL shape drops `auth_url`, rejects empty
  `credential_source` loudly, and gets a field-naming reject via
  `DisallowUnknownFields` (the forbidden fields cannot exist on the struct).
- **NIT 18 (smoke `PREFLIGHT_REQUIRES` mismatch)** → 166/167 `unit-tests`,
  168/169 `live-server`.
- **NIT 19 (methods.go prose counts)** → updated by 168 and 169.

**What the reviews confirmed is right (kept):** the D-025 mutator-vs-re-register
analysis; the admin-gate-by-omission reading; the SSRF-verified allowance;
refusing a general `update_mcp_connection` patch verb; folding
`removeMcpConnection`'s first Svelte caller in; keeping uninstall BREAKING (now
defensible because 167 owner-scopes the reconcile). `SetRawHTMLTrust` is the right structural
template for the mutator — but its AUDIT ordering was a lie, fixed in 166.

---

## 10. Open questions — resolved before dispatch

1. ~~Is "no env-var NAMES on the wire" the invariant?~~ **No** — it is a symptom
   rule. The invariant is "no admin-writable field may determine where a
   credential is sent" (D-300).
2. ~~Where does the token endpoint / downstream-host set live?~~ **Boot-declared
   on the named credential broker / the provider config**; the writable
   descriptor carries ZERO URLs (D-303).
3. ~~Should the registries be identity-keyed by the triple?~~ **No** — two
   reviews proved triple-keying breaks the `mcpDefault` boot deployment,
   doesn't isolate the bare-name catalog, and reverses D-287. The fix is an
   OWNER tag on runtime-adds for reconcile-view scoping only; boot infra stays
   process-global (D-301 extends D-287). No hard dispatch-isolation claim in a
   shared runtime.
4. ~~A registry MUTATOR or a re-register for the live allowance?~~ **A mutator**
   (D-302) — a re-register tears down a working transport and can fail after a
   widening edit. The mutator is bare-name / process-global (D-287; the registry
   is NOT re-keyed), identity-mandatory for auth; owner safety comes from the
   owner-scoped reconcile (D-301), not from keying.
5. ~~Does rollback take live effect?~~ **Yes** — a run-start allowance-reconcile
   leg (FAIL 7); a revisioned write with no live effect is the silent half-write
   this design rejects.
6. ~~Where does the allowance WRITE affordance live?~~ **Single-homed on
   `/agent-config`**; `/mcp-connections` deep-links (resolving connection→agent
   first). D-302.
7. ~~Is the interactive `oauth2` driver Protocol-installable?~~ **No** —
   env-named client secret + browser redirect; config-only. D-303.
8. ~~Any `ProtocolVersion` bump?~~ **No** — three additive methods + additive
   wire/config surfaces; full D-223/D-209 regen.
