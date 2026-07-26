# Phase 211 — owner-scoped MCP registry mutators

## Summary

Completes the rule D-350 established across the sibling MCP-registry verbs it recorded as follow-ups. The wire-reachable per-server connection write (`Registry.SetRawHTMLTrust`, the MCP-App raw-HTML sandbox posture) now resolves under the caller's own verified **tenant** instead of by bare name; `Registry.Deregister` resolves under the caller's own `(tenant, agent)` **owner**, threaded from both of its production callers through the reconcile seam. `RefreshDiscovery` and `Probe` are **classified** — each records only what its own round-trip observed, so both stay bare-name reads and now say so in godoc. Registry READS, tool resolution, and dispatch are unchanged (D-287 / D-301). Zero-wire.

## RFC anchor

- RFC §6.4 — tool catalog and transports (the MCP driver, the registry the attach path registers into, the per-server state the connection verbs write).
- RFC §6.16 — the Agent Registry (the agent registration identity the `(tenant, agent)` owner tag mirrors; `agent_id` is registration metadata, never an isolation key).
- RFC §7 — the Console as a Protocol client (the surface these admin verbs serve, and the sandbox posture the raw-HTML flag governs).

## Briefs informing this phase

- brief 09 — MCP OAuth (the agent-bound authorization model and the identity-mandatory posture the registry writes inherit).
- brief 03 — tools + integrations (the one-catalog bare-name resolution model this phase deliberately does not widen).
- brief 14 — MCP client/host compliance (the host-side connection lifecycle the registry projects, and its binding wording rule for scoped claims).

## Brief findings incorporated

- **brief 09 §"What Harbor must add" item 3** ("Admin-scope authz on agent-bound flows. Only callers with admin scope on the agent's tenant can initiate / complete / revoke `ScopeAgent` flows."). Note the wording: admin scope **on the agent's tenant**. The raw-HTML trust verb already required the admin claim; what it did not require was that the claim's tenant be the target registration's. This phase supplies that half.
- **brief 09 §"What Harbor must add" item 4** ("Identity-mandatory enforcement … fail closed on missing components"). The tenant-scoped resolver refuses an empty tenant at the resolution choke point rather than falling back to the whole registry, mirroring `ownedEntry`'s zero-owner refusal and `RuntimeAddedSources`' zero-owner nil. The guard is dead-defensive on every live path today (`requireIdentity` fires first) and is pinned by a direct test anyway, because an untested unreachable guard is how an inert guard survives.
- **brief 09 §"What Harbor must add" item 7** (the D-025 concurrent-reuse contract: "no cross-identity bleed, no cross-agent bleed, no scope confusion … Test mandatory"). Both scoped writes gain an N=128 two-principal concurrent run against one shared registry under `-race`.
- **brief 03 §5 "Sharp Edges Harbor Must Avoid"** ("Harbor picks one architecture and bakes the correction in" — no parallel modes). The scoped resolvers sit side by side at ONE choke point (`ownedEntry` for the `(tenant, agent)` form, `tenantEntry` for the tenant form) rather than each caller growing its own check. `Deregister` is the single exception and says why in its own godoc: its comparison must be atomic with the delete, and it is not the same comparison anyway (`ownedEntry` refuses the zero owner, which would break the boot loader's own hot-reload replace).
- **brief 14 §4 "What Harbor can honestly claim"** (the binding wording rule: never state an unscoped claim; state the scoped one and let the harness substantiate it). Applied to the guarantee itself: this phase claims "the caller's own tenant, or the deployment's own boot-declared infrastructure", states the boot-declared bound in the godoc, the decision entry and a test, and does not claim a per-agent scope the wire door cannot supply.

## Findings I'm departing from (if any)

None. The bare-name process-global registry (brief 03's one-catalog model, settled in D-287 and re-affirmed in D-301 and D-350) is preserved exactly: resolution, dispatch, and every read projection stay bare-name and deployment-shared. Only the WRITES are scoped — the same direction D-350 already took.

## Goals

- The MCP-App raw-HTML sandbox posture is written on a registration the caller's own tenant owns, or on a boot-declared (deployment-global) one; a registration another tenant owns answers as if it did not exist.
- The refusal discloses nothing: an unregistered name, and a name another tenant owns, are indistinguishable at the registry boundary.
- The write and its compensating revert resolve identically, so an admin write whose audit emit fails is never left observably applied but unrecorded.
- Registration removal lands only on the registration carrying the caller's own owner tag, with the comparison atomic with the delete, and the owner threaded from both production callers.
- `RefreshDiscovery` and `Probe` carry an explicit read classification in godoc, so a later reader cannot mistake their bare-name resolution for an oversight.
- Every guard is mutation-verified to turn a smoke `OK` into a `FAIL` — never a SKIP.

## Non-goals

- Re-keying the MCP registry or the tool catalog by identity. D-287 / D-301 settled that reads, resolution and dispatch stay process-global and bare-name; this phase does not re-litigate it.
- Scoping the boot-declared (zero-owner) registrations' per-server admin preferences. A boot server is declared by the deployment itself, resolves and dispatches by bare name for every session, and has no per-owner home for the flag — refusing the write there would delete the preference rather than scope it. The bound is stated, tested, and recorded as a follow-up rather than silently assumed away.
- Scoping the observation writes (`RecordAuthChallenge`, `RecordScopeShortfall`, `RecordOAuthRequirement`, `RecordReconnect`, `RecordDiscovery`, `recordError`). They record what a round-trip actually observed, are written unsolicited by transport callbacks from any session's ordinary traffic, and are audited in "Risks / open questions".
- Any wire-type, method, error-code, event, or `ProtocolVersion` change. No D-223 / D-209 regeneration.

## Acceptance criteria

- [ ] `Registry.SetRawHTMLTrust` resolves through a tenant-scoped resolver: the owning tenant succeeds, a different tenant answers `ErrServerNotFound` with the live flag untouched, and a boot-declared registration stays writable.
- [ ] The refusal for a registration another tenant owns is identical to the refusal for a name nobody registered.
- [ ] The scoping tenant is read from the ctx identity the method already requires — never taken as a caller-supplied parameter — so the apply and the compensating revert resolve identically.
- [ ] An empty tenant resolves to no registration at all; it never falls back to the whole registry.
- [ ] `Registry.Deregister` takes an `auth.Owner` and removes only a registration carrying that exact tag; the ZERO owner matches boot-declared registrations and nothing else. The comparison runs under the same write lock as the delete.
- [ ] The attach same-name replace and the run-start reconcile detach leg each thread their own owner through to the registry, and `projection.ConnectionDetacher.Detach` carries the owner its `AttachedSources` view was taken under.
- [ ] `RefreshDiscovery` and `Probe` are documented as reads with their bare-name resolution stated as deliberate, and both stay bare-name.
- [ ] Registry reads (`ListServers`, `GetServer`, `ListResources`, `ListPrompts`, `Health`, `OAuthDiscoveryTarget`, `ReadResource`), resolution and dispatch are unchanged.
- [ ] `ProtocolVersion` is unchanged; no method, wire type, error code, or event moves.
- [ ] `scripts/smoke/phase-211.sh` shows `OK ≥ 8`, `SKIP = 0`, `FAIL = 0` against a live preflight build, and FAILS (never SKIPs) when any guard is removed.

## Files added or changed

```text
internal/tools/drivers/mcp/
  registry.go                                   # tenantEntry; SetRawHTMLTrust +
                                                #   Deregister scoped; RefreshDiscovery + Probe classified
  attach.go                                     # the same-name replace threads deps.Owner
  registry_scoped_mutators_test.go              # NEW — tenant scope, owner scope, revert symmetry,
                                                #   two-principal N=128 concurrent runs, reads-stay-bare
  deregister_test.go                            # owner argument at the existing call sites
  attach_reattach_test.go                       # owner argument at the replace-leg call site
internal/runtime/agentcfg/projection/
  projection.go                                 # ConnectionDetacher.Detach carries the owner
  reconcile_test.go                             # fake detacher records the owner it was called with
  reconcile_owner_test.go                       # NEW — the reconcile threads the reconciling owner
internal/runtime/serve/
  mcp_detacher.go                               # Detach owner param -> registry deregister
  mcp_detacher_owner_test.go                    # NEW — production detacher against a real registry
  coverage_test.go, runloop_failures_test.go    # owner argument at the existing call sites
internal/protocol/
  mcp.go                                        # godoc: apply + revert share the verified idCtx
test/integration/
  phase211_scoped_registry_mutators_test.go     # NEW
scripts/smoke/phase-211.sh                      # NEW
docs/plans/phase-211-owner-scoped-registry-mutators.md  # NEW
docs/plans/README.md, docs/decisions.md, docs/glossary.md
docs/skills/use-the-harbor-protocol/SKILL.md    # §18 same-PR surface update
```

## Public API surface

```go
// internal/tools/drivers/mcp

// Tenant-scoped: resolves a registration the caller's own tenant owns, or a
// boot-declared one. The scoping tenant comes from ctx, not from the caller.
// Signature UNCHANGED — the scope is not a caller-supplied parameter.
func (r *Registry) SetRawHTMLTrust(ctx context.Context, name string, trusted bool) (prev bool, err error)

// Owner-scoped: removes only a registration whose owner tag equals owner
// EXACTLY. The zero owner matches boot-declared registrations and nothing else.
func (r *Registry) Deregister(ctx context.Context, name string, owner auth.Owner) error

// internal/runtime/agentcfg/projection
type ConnectionDetacher interface {
    AttachedSources(ctx context.Context, owner auth.Owner) []string
    Detach(ctx context.Context, source string, owner auth.Owner) error
}
```

`tenantEntry` (tenant equality, boot-declared permitted, empty tenant refused) is package-internal to `internal/tools/drivers/mcp` and sits beside the unchanged `ownedEntry`. `Deregister` spells its comparison out inline because it must be atomic with the delete.

## Test plan

- **Unit:**
  - `TestRegistry_SetRawHTMLTrust_TenantScoped` — the owning tenant succeeds; another tenant is refused with `ErrServerNotFound` and the live flag is untouched.
  - `TestRegistry_SetRawHTMLTrust_UnknownAndOtherTenantAnswerAlike` — the two refusals are indistinguishable.
  - `TestRegistry_SetRawHTMLTrust_BootDeclaredStaysWritable` — the stated bound, pinned.
  - `TestRegistry_SetRawHTMLTrust_IdentityMissing` — identity stays mandatory and the flag is unchanged.
  - `TestRegistry_TenantEntry_EmptyTenantResolvesNothing` — the fail-closed default at the choke point, called directly because it is unreachable through the exported surface.
  - `TestRegistry_SetRawHTMLTrust_CompensatingRevertResolvesSymmetrically` — apply then revert on the same ctx both resolve; a refused apply never reaches a revert.
  - `TestRegistry_Deregister_OwnerScoped` / `..._ZeroOwnerMatchesOnlyBootDeclared` — cross-owner removal is refused with the transport never closed; the zero owner reaches boot state and nothing else; a half owner reaches neither.
  - `TestRegistry_ReadsStayBareName` — every read projection, plus `RefreshDiscovery` and `Probe`, still resolves another tenant's and a boot-declared registration by bare name.
  - `TestReconcileConnections_PassesReconcilingOwnerToDetach` — the reconcile leg carries the owner its view was taken under.
  - `TestMCPConnectionDetacher_Detach_OwnerScoped` / `..._NeverRemovesBootDeclared` — the PRODUCTION detacher against a REAL registry.
- **Integration:** `test/integration/phase211_scoped_registry_mutators_test.go` — the real process-global `mcp.Registry`, the real `mcpconsole.RegistryAccessor`, the real `protocol.MCPSurface` (identity gate + body-scope reconciliation + the admin-write/audit unit), a real in-memory bus and a real patterns redactor, and the real REST control transport over an `httptest.Server`. Covers the cross-tenant write refused as `not_found` with the owning tenant's flag intact, the indistinguishable-refusal property, the owning tenant's write landing, the boot-declared name staying writable, the **compensating-revert failure mode** (a closed bus makes the audit emit fail for real; the error must report the reverted-not-applied case and the prior value must be restored), a missing-identity request over the real transport, and an N=16 concurrent cross-tenant stress.
- **Conformance:** N/A — no new driver interface; the MCP registry is a single process-local concrete.
- **Concurrency / leak:** `TestRegistry_SetRawHTMLTrust_ConcurrentTenants` and `TestRegistry_Deregister_ConcurrentOwners` (N=128 per principal, two principals, ONE shared registry, under `-race`), plus `TestE2E_P211_ConcurrentCrossTenantWriters` (N=16 through the real surface). The deregister run additionally asserts exactly one removal lands and the transport closes exactly once — the delete is atomic with the owner comparison.

## Smoke script additions

`scripts/smoke/phase-211.sh` (`live-server`):

- Static: the tenant-scoped resolver is declared; `SetRawHTMLTrust` resolves under `id.TenantID` and no longer calls `r.entry(name)` inside its own body; `Deregister` takes an owner and compares it before the delete; the attach replace, the projection seam and the production detacher each thread the owner; **both** `RefreshDiscovery` and `Probe` carry the read classification (counted, so deleting one still fails); the read projection keeps its bare-name signature; `ProtocolVersion` is unchanged.
- Live: `mcp.servers.set_raw_html_trust` is identity-mandatory (no bearer → a typed refusal, the flag untouched), and a name the caller's scope does not resolve fails loud with a typed `404 not_found` — the SAME answer the tenant-scoped resolver gives for a registration another tenant owns, so the refusal shape is exercised against the live surface. `mcp.servers.list` still answers 2xx with a `.servers` body (the reads stay bare-name).
- **The route probe uses an EMPTY body, and that is load-bearing.** The first draft probed with a real request and treated any 404 as "route not present" — but this verb answers a genuine unregistered name with `404 not_found`, so the probe converted a real answer into a SKIP. It was caught by running the smoke against the live preflight server and reading the SKIP rather than the summary: precisely the §4.2 item 5 shape ("a SKIP that should be an OK is a bug") this phase exists to close, reproduced inside the phase's own gate. An empty body separates the cases cleanly — a mounted route answers `400 invalid_request` ("name is required"); an unmounted / unknown method answers `404 unknown_method` — and the classification now reads the typed `code`, never the bare status. The same run also disproved a second assumption: the preflight dev token DOES carry the admin claim, so the originally-planned `scope_mismatch` assertion would have been wrong.
- Guard tests: four `go test -race -run` legs (registry scoped writes; the reconcile owner threading; the production detacher against a real registry; the protocol → tools/mcp integration seam). Each FAILS on a genuine failure and only SKIPs when the filter matches no tests at all.
- The live block degrades to a SKIP on its own rather than exiting the script, so the guard legs run on a standalone invocation too.
- Against the live preflight server: **OK 18 / SKIP 0 / FAIL 0**. Standalone (no server): OK 15 / SKIP 1 / FAIL 0, the one SKIP being the live block.

Verified empirically — eight mutations, each turning a corresponding `OK` into a `FAIL` with SKIP unchanged: the tenant comparison; the empty-tenant refusal; the `SetRawHTMLTrust` resolver choice; the `Deregister` owner comparison; the attach replace's owner threading; the reconcile leg's owner threading; the production detacher's owner threading; and deleting ONE of the two read classifications.

## Coverage target

- `internal/tools/drivers/mcp`: 85% — measured **85.7%**.
- `internal/runtime/serve`: 70% — measured **84.4%** (the detacher's owner parameter is covered).
- `internal/runtime/agentcfg/projection`: 80% (the target phase 206 set for this package) — measured **81.0%**.
- `internal/protocol`: not a target for this phase — the only change here is eight lines of godoc, which contain no statements, so the package's measured **76.4%** is exactly its pre-existing figure and cannot have moved. It sits below the 80–85% other plans state for the package; that shortfall predates this phase and is not addressed by it.

## Dependencies

- 206 (D-350 — the owner-scoped resolution this phase completes across the siblings; it built `ownedEntry` and applied it to the first verb).
- 205 (D-349 — the shared body-scope gate that pins the caller's triple, so the tenant the resolver reads is the caller's own verified tenant).
- 167 / 168 (D-301 / D-302 — the owner tag and the reconcile view the write scope extends).

## Risks / open questions

- **A boot-declared registration's per-server admin preferences remain writable by any admin in the deployment.** This is the stated bound, not an oversight: the flag has no per-owner home on a boot-declared server and no other door can set it, so refusing the write would remove the capability rather than scope it. Narrowing it would need a new operator-facing policy (which tenants may write which boot connections' preferences) — a config surface, not a resolution rule. Recorded as a follow-up.
- **The wire door supplies a tenant, not a `(tenant, agent)` owner.** `types.MCPServerSetRawHTMLTrustRequest` carries only the identity triple and this phase is zero-wire, so the strongest comparison available is the tenant. Within one tenant, co-tenant admins already share the runtime-added connection namespace by construction (D-301's stated bound), so the tenant is the boundary that matters — but if a future phase adds an agent id to this family's requests, the resolver should move to `ownedEntry` and this note is the pointer.
- **The observation writes stay bare-name and unscoped, deliberately** (`RecordAuthChallenge` `internal/tools/drivers/mcp/registry.go:852`, `RecordScopeShortfall` :871, `RecordOAuthRequirement` :885, `RecordReconnect` :1196, `RecordDiscovery` :1257, `recordError` :1183). All six record what a transport round-trip actually observed; five are called only from transport callbacks or the boot attach path, and `RecordOAuthRequirement` is reachable from the wire exactly once, through `mcp.servers.probe` → `mcpconsole.RegistryAccessor.maybeDiscoverOAuthRequirement` (`internal/mcpconsole/mcpconsole.go:224`), where it records the requirement the probe's own discovery walk returned. None persists a caller-chosen value and none is consulted as policy on a later authorization decision, so scoping them would refuse a boot server's observation while the identical state kept being written by ordinary traffic. Audited, deliberately not changed.
- **`Registry.Register` is not owner-scoped and does not need to be**, but the reason is caller-side: the same-name replace's owner comparison lives in `Attach` (`internal/tools/drivers/mcp/attach.go:246`, via `OwnerOf`) rather than at the registry's own choke point, so a hypothetical future caller reaching `Register` directly could replace another owner's entry. `Deregister` moved its guard to the choke point in this phase; `Register`'s is a larger change (the upsert semantics differ) and is recorded as a follow-up rather than bundled here.
- **`Detach`'s cross-owner refusal is swallowed as already-detached.** `MCPConnectionDetacher.Detach` treats `ErrServerNotFound` as idempotent success, which is correct for the reconcile (a source already gone is a no-op) but means a cross-owner detach is silent rather than loud. It is unreachable in production — the reconcile only ever enumerates the owner's own view — and making it loud would break the idempotency the reconcile depends on. Pinned by test (the registration and its transport survive) rather than changed.

## Glossary additions

- **Tenant-scoped connection write** — added to `docs/glossary.md` in this PR.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes
- [x] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. `TestRegistry_SetRawHTMLTrust_ConcurrentTenants` and `TestRegistry_Deregister_ConcurrentOwners` each run N=128 per principal against one shared `Registry`.
- [x] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md §17. `test/integration/phase211_scoped_registry_mutators_test.go`.
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: justified above + decisions.md entry filed (none departed)
