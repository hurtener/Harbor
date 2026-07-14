# Phase 167 — Identity-keyed MCP + provider registries

> Part of the v1.14 wave (`docs/plans/wave-v114-coordination.md`). Decision:
> **D-301**. A shipped-code correctness fix that makes BOTH tenancy topologies
> safe (one runtime per tenant OR one runtime serving many tenants) and
> unblocks the write phases (168/169). Depends on Phase 166.

## Summary

Harbor is a FRAMEWORK: a downstream team may run one runtime per tenant OR one
runtime serving many tenants — both are supported. But the MCP connection
registry and the OAuth-provider set are keyed by BARE NAME in process-global
maps, while authority is per-`(tenant, user, session)`. `mcpdrv.Registry.servers`
is a `map[string]*serverEntry` (`internal/tools/drivers/mcp/registry.go:273-275`)
with a blind overwrite at `:353`; the provider set is a by-value
`map[string]auth.OAuthProvider` handed into the attach path
(`internal/tools/drivers/mcp/attach.go:288`) and the catalog builder. In the
multi-tenant topology this is a live cross-tenant bug: tenant B installing or
adding a name that tenant A already uses overwrites A's entry; B's removal
closes A's; and B merely STARTING A RUN reconciles B's (empty) declared set
against the process-global live set and detaches A's servers. The code already
concedes the debt — `internal/runtime/agentcfg/projection/projection.go:125-132`
and `internal/runtime/serve/mcp_detacher.go:77-87` both NOTE the enumeration is
"process-global … the future multi-agent attach leg must scope the attached set
per agent." That note is now due. This phase keys both registries by the
identity triple, threads identity through `AttachDeps` / `resolveOAuthBinding` /
the reconcile seam, fails closed on missing identity (§6.9), and closes the two
NOTEs — making the write phases that follow safe under both topologies.

## RFC anchor

- RFC §6.4 — the tool catalog + the MCP registry being keyed.
- RFC §6.9 — sessions and the identity-mandatory posture (fail closed on a
  missing triple).
- RFC §6.16 — the agent-config control plane whose reconcile seam this scopes.
- RFC §7 — the isolation the keying enforces (no cross-tenant read/write/detach).

## Briefs informing this phase

- brief 05
- brief 03

## Brief findings incorporated

- **brief 05 §"State, tasks, sessions — identity is the storage key":** every
  identity-scoped store filters by the triple; "no fetch-all-then-filter, no
  current-identity-from-a-global." The MCP registry and provider set are exactly
  such stores (they hold live, identity-scoped runtime entities) yet key by bare
  name — this phase brings them onto the triple like every other store.
- **brief 05 §"multi-isolation is a Day-1 guarantee, not a mode":** a runtime
  serving many tenants must isolate them by construction. The process-global
  maps break that guarantee the moment two tenants collide on a name; the two
  in-code NOTEs are the deferred acknowledgement this phase settles.
- **brief 03 §"tool catalog + transports":** the catalog and its transports are
  compiled artifacts shared across runs (D-025); per-run/per-identity state
  lives in `ctx`, never on the artifact. The registry keying makes identity a
  first-class lookup dimension rather than a global the artifact reads from
  itself — aligning with the concurrent-reuse contract.

## Findings I'm departing from (if any)

None.

## Goals

- **Key `mcpdrv.Registry` by the identity triple.** `servers` becomes keyed by
  `(triple, name)` (the innermost map keyed by name under a per-triple bucket,
  or a composite key — implementor's call, pinned by the concurrency test).
  `Register` no longer blind-overwrites across tenants; every read/mutate
  (`entry`, `OAuthDiscoveryTarget`, `SetRawHTMLTrust`, `ServerView`, the
  challenge/requirement records, `SourceIDs`) takes the triple and scopes to it.
  Identity is MANDATORY — a call with a missing triple component is a loud typed
  error (`requireIdentity` already exists at the registry edge; extend it to the
  keyed lookups), never a process-global fallback (§6.9, fail closed).
- **Key the provider set by the identity triple.** The by-value
  `map[string]auth.OAuthProvider` in `AttachDeps` / the catalog builder becomes
  an identity-scoped lookup: a provider named "acme" for tenant A is a distinct
  entry from tenant B's "acme". `resolveOAuthBinding` takes the triple and
  resolves within it. (This phase keys the EXISTING boot-seeded set; the
  runtime-installable provider set — Phase 169 — is built on top and inherits
  the keying for free.)
- **Thread identity through the reconcile seam.** `AttachedSources` /
  `ReconcileConnections` (`projection.go:141`) + the `MCPConnectionDetacher`
  (`mcp_detacher.go`) enumerate and detach WITHIN the reconciling run's triple,
  never process-globally. A tenant starting a run reconciles only its own
  declared-vs-attached set. This closes the two NOTEs verbatim.
- **Fail closed on missing identity everywhere the keying reaches.** No
  identity-downgrading fallback, no "process-global for dev" escape hatch in the
  keyed paths (dev supplies a real default identity already; the keyed lookup
  uses it like any other triple). §6 rule 9: the runtime fails closed.
- **Preserve the single-runtime-per-tenant topology unchanged in behaviour.**
  With one tenant, the keyed maps behave exactly as the name-keyed maps did
  (one bucket) — no regression for that deployment shape; the keying is what
  makes the many-tenants shape correct without a separate code path (§13: one
  mechanism, both topologies).

## Non-goals

- **No new Protocol surface.** This is internal keying + the reconcile-seam
  threading; no method, wire type, or event.
- **No runtime-installable providers** (Phase 169) and **no discovery-allowance
  write** (Phase 168) — both build on the keyed registries this phase ships.
- **No change to the credential-sink hardening** (Phase 166) — that is
  boot-declared and topology-independent; this phase is orthogonal and stacks
  on it.
- **No agent_id in the isolation key.** The isolation boundary stays
  `(tenant, user, session)` (CLAUDE.md §6 clarifying note); `agent_id` is a
  runtime entity id, never an isolation filter. Where the reconcile seam scopes
  "per agent" in the NOTEs, that is a per-agent VIEW within the triple, not a
  widening of the isolation key.

## Acceptance criteria

- [ ] `mcpdrv.Registry` is keyed by the identity triple: `Register` for the same
      name under two different triples creates TWO entries; a read/mutate under
      triple A never observes or mutates triple B's entry. Test:
      `TestRegistry_IdentityKeyed_NoCrossTenantOverwrite` (two triples, same
      name; independent state).
- [ ] Every registry read/mutate (`OAuthDiscoveryTarget`, `SetRawHTMLTrust`,
      `ServerView`, `RecordAuthChallenge`, `RecordOAuthRequirement`,
      `SourceIDs`) takes the triple and fails closed on a missing component with
      a typed error. Test: `TestRegistry_MissingIdentity_FailsClosed`.
- [ ] The provider set is identity-scoped: `resolveOAuthBinding` resolves the
      provider within the caller's triple; tenant A's "acme" and tenant B's
      "acme" are distinct. Test:
      `TestResolveOAuthBinding_ProviderScopedByIdentity`.
- [ ] `ReconcileConnections` / `AttachedSources` / `MCPConnectionDetacher`
      enumerate and detach WITHIN the reconciling run's triple; a tenant B run
      never detaches a tenant A server. The two "process-global enumeration"
      NOTEs (`projection.go:125-132`, `mcp_detacher.go:77-87`) are removed and
      the code matches. Test:
      `TestReconcile_ScopedToTriple_NeverDetachesOtherTenant`.
- [ ] **Cross-tenant isolation integration test (mandatory, §6 rule 10):** N
      concurrent runs across ≥2 tenants adding / removing / reconciling MCP
      connections and resolving provider bindings of the SAME name, asserting
      zero cross-talk (no overwrite, no cross-detach, no cross-provider
      resolution) under `-race`.
- [ ] The single-tenant topology is behaviour-identical (a regression suite of
      the existing registry tests passes unchanged, modulo the added triple
      argument).
- [ ] `scripts/smoke/phase-167.sh` OK ≥ 2, FAIL = 0.
- [ ] `-race` green; coverage ≥ the stated target on every touched Go package.

## Files added or changed

- `internal/tools/drivers/mcp/registry.go` — the triple-keyed `servers` map +
  every read/mutate signature; `requireIdentity` extended to the keyed lookups.
- `internal/tools/drivers/mcp/attach.go` — `AttachDeps` carries the triple;
  `resolveOAuthBinding` resolves the provider within it.
- `internal/runtime/serve/mcp_attacher.go` + `mcp_detacher.go` — thread the
  triple; scope `AttachedSources` to it; remove the NOTE.
- `internal/runtime/agentcfg/projection/projection.go` — `ReconcileConnections`
  scopes to the run's triple; remove the NOTE.
- `internal/mcpconsole/mcpconsole.go` — the accessor passes the triple through
  (it already carries identity on its calls).
- `test/integration/phase167_identity_keyed_registries_test.go` (new).
- `scripts/smoke/phase-167.sh` (new); `docs/plans/README.md`;
  `docs/decisions.md` (D-301); `docs/glossary.md`.

## Public API surface

```go
// internal/tools/drivers/mcp — every registry method gains the triple (identity
// mandatory; a missing component is a loud typed error). Illustrative:
//   func (r *Registry) OAuthDiscoveryTarget(id identity.Identity, name string) (*AuthChallenge, string, []string, error)
//   func (r *Registry) SetRawHTMLTrust(ctx context.Context, name string, trusted bool) (bool, error) // already ctx-scoped; now triple-keyed
//   func (r *Registry) SourceIDs(id identity.Identity) []string
// No Protocol/wire change.
```

## Test plan

- **Unit:** the no-cross-tenant-overwrite table; missing-identity fail-closed on
  every keyed method; provider resolution scoped by triple; the reconcile scope
  (a triple-B reconcile never enumerates triple-A sources).
- **Integration (`test/integration/phase167_identity_keyed_registries_test.go`)
  — binding per §17.1:** real MCP driver + real agent-config reconcile across
  ≥2 tenants — same-name connections and providers stay isolated through add /
  remove / reconcile / bind; a tenant-B run never detaches a tenant-A server;
  identity propagation; ≥1 failure mode (missing identity → loud); `-race`.
- **Conformance:** the existing MCP registry conformance/behaviour suite runs
  under the keyed shape (single-tenant equivalence proof).
- **Concurrency / leak:** N≥100 concurrent operations across ≥2 tenants against
  ONE shared `Registry` under `-race` — no torn map, no cross-tenant bleed, no
  goroutine leak after teardown (D-025).

## Smoke script additions

`scripts/smoke/phase-167.sh` — classification `unit-tests`:

- `go test -race` the registry keying + reconcile-scope packages
  (`TestRegistry_IdentityKeyed_NoCrossTenantOverwrite`,
  `TestReconcile_ScopedToTriple_NeverDetachesOtherTenant`).
- Static: `grep` that the two "process-global enumeration" NOTEs are GONE from
  `projection.go` and `mcp_detacher.go` (the debt-closed trip-wire).
- Done-definition: `OK ≥ 2`, `FAIL = 0`.

## Coverage target

- `internal/tools/drivers/mcp` (the keyed registry): 85%
- `internal/runtime/agentcfg/projection` (the scoped reconcile): 85%
- `internal/runtime/serve` (the scoped detacher/attacher): existing target

## Dependencies

- **166** (the credential-sink hardening this stacks on — the wave builds
  bottom-up).
- **28** (the MCP registry), **92f** (the add path), **156** (D-287 — the
  reconcile seam being scoped), **92a** (the agent-config registry).

## Risks / open questions

- **Signature churn.** Threading the triple through every registry method is a
  wide, mechanical change touching many call sites; the risk is an unkeyed call
  slipping through. Mitigation: `requireIdentity` fails closed, so an unkeyed
  path fails LOUD at test time rather than silently falling back to a global —
  and the drift is caught by the missing-identity test on every method.
- **The reconcile-scope change interacts with Phase 156's detach-only
  reconcile.** Scoping the enumeration to the triple must not resurrect the
  D-287 windows 156 documented; the integration test asserts the same
  detach-only semantics, now per-triple. If a genuinely new window opens, it is
  documented with a bound in the same PR (§17.6).
- **Single-tenant equivalence must be proven, not assumed.** The existing
  registry tests are the regression guard; any behaviour delta for the
  one-tenant deployment is a bug, not an accepted deviation.
- **`SourceIDs` / `AttachedSources` callers.** Every caller of the formerly
  process-global enumeration must now supply a triple; a caller with no natural
  triple (a genuine fleet/admin enumeration) uses the verified admin-scope
  widening (D-284/D-299), never an unkeyed read — enumerated in the PR.

## Glossary additions

- **Identity-keyed registry** — an internal runtime registry (the MCP
  connection registry `mcpdrv.Registry`; the OAuth-provider set) keyed by the
  `(tenant, user, session)` triple rather than a bare name, so a Harbor runtime
  serving MULTIPLE tenants isolates their same-named connections and providers
  by construction — no cross-tenant overwrite, no cross-tenant removal, no
  cross-tenant reconcile/detach. Identity is mandatory: a keyed lookup with a
  missing triple component fails closed with a typed error (§6 rule 9). Ships in
  Phase 167 (D-301), closing the two "process-global enumeration" NOTEs the
  reconcile seam carried, and is the base the runtime-write phases (168/169)
  stand on. RFC §6.4, §6.9, §7, D-301.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] **If multi-isolation code paths changed: cross-session/cross-tenant
      isolation test passes** — the mandatory N-tenant concurrent
      add/remove/reconcile/bind isolation test (§6 rule 10)
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent operations across ≥2
      tenants against ONE shared `Registry` under `-race`; no torn map, no
      cross-tenant bleed, no goroutine leak (D-025)
- [ ] **Integration test exists**
      (`test/integration/phase167_identity_keyed_registries_test.go`), wires
      real drivers end-to-end across ≥2 tenants, asserts identity propagation +
      isolation, covers ≥1 failure mode (missing identity → loud), runs under
      `-race`
- [ ] Single-tenant equivalence proven (existing registry tests pass under the
      keyed shape)
- [ ] §18 skill hygiene: internal keying touches no operator-followed surface —
      exempt (grep confirms no skill names the registry keying); noted in the PR
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: N/A — none departed
