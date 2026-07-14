# Phase 167 — Owner-scoped reconcile for runtime-added connections + providers

> Part of the v1.14 wave (`docs/plans/wave-v114-coordination.md`). Decision:
> **D-301**. A NARROW shipped-code correctness fix that scopes the run-start
> reconcile VIEW to the reconciling owner's runtime-added entries — NOT a
> registry/catalog re-key. It EXTENDS D-287 (it does not reverse it) and is the
> base the write phases' reconcile legs (168/169) stand on. Depends on Phase 166.

## Summary

Harbor is a FRAMEWORK: a downstream team runs one runtime per tenant OR one
runtime serving many. Boot-declared MCP servers attach ONCE under a single
deployment identity (`mcpDefault`, `internal/runtime/assemble/assemble.go:929`)
and their tools live in the process-global, bare-name tool catalog
(`internal/tools/catalog.go` — `byName`, `Resolve(name)`, NOT
identity-filtered); reads and dispatch happen under many session triples. This
is D-287's settled, PR-464-hardened model ("the catalog + MCP registry are
shared across sessions … a refcount/drain protocol was considered and
rejected"). This phase does NOT change it. What it fixes is a narrow latent bug
the two in-code NOTEs already flag (`projection.go:125-132`,
`mcp_detacher.go:77-87`): the run-start reconcile enumerates the process-global
registry, so in a multi-OWNER deployment (several agents doing runtime-adds) one
owner's run reconciles its declared set against ALL attached sources and
DETACHES boot servers and other owners' runtime-added connections. The fix is an
OWNER TAG on runtime-added connections + Protocol-installed providers, used for
exactly one thing beyond the revision ownership they already carry: scoping the
reconcile VIEW so a run only ever touches ITS OWN owner's runtime-added
entries — never boot servers, never another owner's adds. This is precisely what
the NOTEs asked for ("scope the attached set per agent") — a per-owner reconcile
view, NOT full-triple isolation keying (and §6 says `agent_id` is not an
isolation key).

## RFC anchor

- RFC §6.4 — the tool catalog + the MCP registry (whose reconcile VIEW is
  scoped; the shared bare-name model is unchanged).
- RFC §6.16 — the agent-config control plane + the run-start reconcile seam this
  scopes.
- RFC §7 — the isolation posture (stated HONESTLY: the bounded guarantee, not a
  false hard-isolation claim).

## Briefs informing this phase

- brief 05
- brief 03

## Brief findings incorporated

- **brief 05 §"State, tasks, sessions — identity is the storage key":** an
  identity-scoped store filters by the triple. Applied HONESTLY here: the
  agent-config REVISION store already does this (`ConfigScopeAgent`); the runtime
  tool registry + catalog are deliberately NOT identity-scoped stores (they hold
  deployment-shared runtime entities, D-287), so this phase scopes the reconcile
  VIEW by owner tag rather than re-keying the store — the smallest change that
  fixes the over-detach without breaking the shared-catalog model.
- **brief 05 §"multi-isolation is a Day-1 guarantee, not a mode":** the isolation
  boundary is the triple, and `agent_id` is NOT part of it (CLAUDE.md §6
  clarifying note). This phase honours that: the owner tag `(tenant, agent)` is a
  reconcile-VIEW filter, never an isolation principal, and the phase claims NO
  hard cross-tenant isolation of runtime-added tool DISPATCH in a shared runtime
  (it cannot — the catalog is bare-name; see Non-goals).
- **brief 03 §"tool catalog + transports":** the catalog is a compiled artifact
  shared across runs (D-025); per-run state lives in `ctx`. The owner tag lives
  on the runtime-added registry ENTRY (set at attach), read by the reconcile —
  not on the compiled catalog, which stays untouched.

## Findings I'm departing from (if any)

- **This phase DEPARTS from the earlier v1.14 draft's "identity-key the
  registries by the (tenant, user, session) triple" instruction**, which two
  delta reviews returned NO-GO on. Full-triple keying (a) BREAKS the common
  deployment — boot servers attach under one `mcpDefault` identity but are read
  under many session triples, so triple-keying fragments deployment-wide servers
  into per-session buckets and they vanish from the Console / posture for real
  sessions; (b) does NOT isolate — dispatch + bearer injection go through the
  process-global bare-name catalog (`catalog.go`), which Phase 169 explicitly
  declines to widen, so keying registry METADATA leaves same-named runtime-added
  connections colliding (`ErrToolDuplicateName`) or cross-serving; and (c)
  silently reverses D-287. The corrected model (owner-tag runtime-adds; boot
  infra stays deployment-global) is recorded in D-301 as an EXTENSION of D-287.

## Goals

- **Owner-tag runtime-added connections + Protocol-installed providers.** A
  runtime-added connection's registry entry (and a Phase 169 installed provider)
  carries an OWNER tag `(tenant, agent)` — the same owner that owns its
  agent-config revision (`ConfigScopeAgent`). Boot-declared servers/providers are
  UNTAGGED (or tagged `boot`) and are NEVER reconcilable. The tag is set at the
  runtime-add attach (`MCPConnectionAttacher.Attach` already carries the caller
  identity via `AttachRequest.Identity`).
- **Scope the reconcile VIEW by owner (the fix the two NOTEs asked for).**
  `AttachedSources` / `ReconcileConnections` (`projection.go:141`) +
  `MCPConnectionDetacher` (`mcp_detacher.go`) enumerate ONLY the reconciling
  run's owner's runtime-added entries — never boot servers, never another
  owner's adds. A tenant-B run's reconcile can no longer detach a tenant-A
  runtime-added server or a boot server. This is a per-owner reconcile VIEW, not
  a store re-key.
- **Boot infra stays process-global; D-287 is PRESERVED (goal, not side
  effect).** The boot MCP registry, the bare-name tool catalog, and dispatch are
  UNCHANGED. Boot-declared servers stay visible to every session's Console /
  posture read exactly as today. No refcount, no drain, no per-session
  fragmentation.
- **State the bounded guarantee HONESTLY (no false safety property).** In a
  SHARED runtime, runtime-added connection NAMES share a deployment namespace: a
  name collision fails loud (`ErrToolDuplicateName`), and this phase does NOT
  claim hard cross-tenant isolation of runtime-added tool dispatch. A deployment
  needing hard isolation of runtime-added tools runs ONE-RUNTIME-PER-TENANT —
  which then gets full isolation for free (one tenant; everything in the global
  catalog is theirs). Documented in the plan, D-301, and the Console copy where
  a runtime-add is offered.
- **Fail closed on a missing owner tag where the reconcile relies on it** — a
  runtime-added entry with no owner is a loud error at attach, never a
  silently-unreconcilable orphan.

## Non-goals

- **NO identity-keying of the boot MCP registry or the tool catalog.** They stay
  process-global bare-name (D-287). This phase touches the reconcile VIEW, not
  the store.
- **NO cross-tenant isolation of runtime-added tool DISPATCH in a shared
  runtime.** The catalog is bare-name and Phase 169 declines to widen it; a
  false isolation claim is exactly the FAIL-A the reviews caught. The real,
  bounded guarantee is stated above.
- **NO change to boot-server visibility.** Boot servers remain visible to every
  session — the property full-triple keying would have broken.
- **NO new Protocol surface.** Internal owner-tag + reconcile-view scoping; no
  method, wire type, or event.
- **NO `agent_id` in the isolation key.** The owner tag is a reconcile-view
  filter (§6 clarifying note); the isolation boundary stays the triple.

## Acceptance criteria

- [ ] A runtime-added connection's registry entry carries an owner tag
      `(tenant, agent)` set at attach from the verified `AttachRequest.Identity`;
      a boot-declared server is untagged and never enumerated as reconcilable.
      Test: `TestRegistry_RuntimeAddCarriesOwnerTag_BootUntagged`.
- [ ] **Boot servers stay visible to every session (the property triple-keying
      broke):** a boot-declared server registered under `mcpDefault` is returned
      by a posture/`ServerView` read under an ARBITRARY session triple. Test:
      `TestRegistry_BootServerVisibleToEverySession` (replaces the discredited
      `TestRegistry_IdentityKeyed_NoCrossTenantOverwrite`, which used two
      tenant-differing triples that could not distinguish tenant-key from
      full-triple-key — reviewer FAIL-B).
- [ ] **Owner-scoped reconcile:** a reconcile run for owner A enumerates and
      detaches ONLY owner A's runtime-added entries — never a boot server, never
      owner B's runtime-added entry. Test:
      `TestReconcile_OwnerScoped_NeverDetachesBootOrOtherOwner`.
- [ ] The Phase 169 provider set carries the same owner tag and its reconcile
      leg is owner-scoped identically (a tenant-B run never uninstalls tenant-A's
      installed provider). (Pinned here as the shared primitive; exercised end
      to end in 169.)
- [ ] The bounded guarantee is documented: a shared-runtime runtime-add name
      collision fails loud (`ErrToolDuplicateName`); the phase claims NO dispatch
      isolation. A test asserts the loud collision (not a silent overwrite, not a
      false success). Test:
      `TestRuntimeAdd_SharedRuntime_NameCollisionFailsLoud`.
- [ ] D-287's process-global catalog/registry/dispatch model is UNCHANGED — the
      existing D-287 detach + shared-catalog tests pass unmodified; the two
      in-code NOTEs are REWRITTEN (not deleted) to describe the deliberate
      process-global boot behaviour PLUS the new owner-scoped reconcile view.
- [ ] `scripts/smoke/phase-167.sh` OK ≥ 2, FAIL = 0.
- [ ] `-race` green; coverage ≥ the stated target on every touched Go package.

## Files added or changed

- `internal/tools/drivers/mcp/registry.go` — the owner tag on a runtime-added
  `serverEntry` (boot entries untagged); an owner-filtered `SourceIDs` /
  reconcile-view accessor (the bare-name read paths + dispatch are UNCHANGED).
- The `Owner` tag type this phase establishes is consumed by Phase 169's
  `internal/tools/auth/providers.go` (`ProviderSet`) — that file is CREATED in
  169, not here; this row is a forward reference so the tag's single consumer is
  visible (§13). This phase defines the tag; 169 adds the owner-filtered
  provider enumeration on top of it.
- `internal/runtime/serve/mcp_attacher.go` — stamp the owner tag from
  `AttachRequest.Identity` at runtime-add attach.
- `internal/runtime/serve/mcp_detacher.go` — `AttachedSources` scoped to the
  reconciling owner; the NOTE rewritten.
- `internal/runtime/agentcfg/projection/projection.go` — `ReconcileConnections`
  enumerates the owner's runtime-added set only; the NOTE rewritten.
- `test/integration/phase167_owner_scoped_reconcile_test.go` (new).
- `scripts/smoke/phase-167.sh` (new); `docs/plans/README.md`;
  `docs/decisions.md` (D-301); `docs/glossary.md`.

## Public API surface

```go
// internal/tools/drivers/mcp — runtime-added entries gain an owner tag; the
// reconcile-view enumeration filters by it. The bare-name read/dispatch paths
// (Resolve, ServerView, OAuthDiscoveryTarget) are UNCHANGED — boot servers stay
// globally visible. Illustrative:
//   type Owner struct { Tenant, Agent string } // reconcile-view tag, NOT an isolation key
//   func (r *Registry) RuntimeAddedSources(owner Owner) []string // owner-scoped; excludes boot
// No Protocol/wire change.
```

## Test plan

- **Unit:** the owner tag set-at-attach + boot-untagged; boot visibility under an
  arbitrary session triple; owner-scoped reconcile view (owner A never sees
  boot / owner B); the loud name-collision in a shared runtime; the
  missing-owner-tag fail-closed at attach.
- **Integration (`test/integration/phase167_owner_scoped_reconcile_test.go`) —
  binding per §17.1:** real MCP driver + real agent-config reconcile with a boot
  server + two owners' runtime-adds — owner A's run-start reconcile detaches only
  A's undeclared add, leaves the boot server and B's add attached; boot server
  visible to a C-session posture read; a same-name runtime-add by B while A holds
  it fails loud; identity propagation; `-race`.
- **Conformance:** the existing D-287 detach + shared-catalog behaviour suite
  runs UNCHANGED (the preservation proof).
- **Concurrency / leak:** N≥100 concurrent reconciles across ≥2 owners + boot
  reads against ONE shared `Registry` under `-race` — owner-scoped detaches
  never touch boot / another owner; no torn state; no goroutine leak (D-025).

## Smoke script additions

`scripts/smoke/phase-167.sh` — classification `unit-tests`:

- `go test -race` the owner-tag + owner-scoped-reconcile packages
  (`TestReconcile_OwnerScoped_NeverDetachesBootOrOtherOwner`,
  `TestRegistry_BootServerVisibleToEverySession`).
- Static: `grep` that the two reconcile NOTEs now describe the OWNER-SCOPED view
  (the corrected trip-wire — the design KEEPS a rewritten note about deliberate
  process-global boot behaviour, so the earlier "the NOTEs are GONE" assertion
  would INVERT; assert the note MENTIONS the owner-scoped reconcile instead).
- Done-definition: `OK ≥ 2`, `FAIL = 0`.

## Coverage target

- `internal/tools/drivers/mcp` (the owner tag + owner-scoped enumeration): 85%
- `internal/runtime/agentcfg/projection` (the owner-scoped reconcile): 85%
- `internal/runtime/serve` (the attach-time tag stamp + scoped detacher):
  existing target

## Dependencies

- **166** (the wave builds bottom-up on the hardened credential edge).
- **28** (the MCP registry), **92f** (the add path + `AttachRequest.Identity`),
  **156** (D-287 — the reconcile seam being scoped, EXTENDED not reversed),
  **92a** (the agent-config revision ownership the tag mirrors).

## Risks / open questions

- **The bounded guarantee must be stated, not implied.** The single biggest
  review finding was a FALSE isolation claim. The plan, D-301, and the Console
  runtime-add copy all state plainly: a shared runtime trusts co-tenant admins
  for runtime-added connection names; hard isolation = one-runtime-per-tenant.
  The adversarial review verifies NO test or doc asserts cross-tenant dispatch
  isolation.
- **Owner tag vs isolation key.** The tag is `(tenant, agent)` and is used ONLY
  for reconcile-view scoping + the revision ownership it mirrors — never in a
  `WHERE` clause as an isolation filter, never for dispatch. The review checks
  no code path treats it as an isolation principal (§6).
- **D-287 preservation is a test obligation, not a claim.** The existing D-287
  detach + shared-catalog tests run unmodified; any behaviour delta for the
  single-agent / boot-server case is a bug, not an accepted deviation.
- **Does this still merit a standalone phase?** Yes — it is a shared reconcile
  primitive with THREE consumers (the shipped D-287 detach leg it fixes, Phase
  168's allowance reconcile, Phase 169's provider reconcile) and it fixes a
  latent multi-owner over-detach independently of the new writes. Folding it into
  168 would bury a primitive 169 depends on. See the wave doc §5.

## Glossary additions

- **Owner tag (runtime-added entry)** — a `(tenant, agent)` tag on a
  runtime-added MCP connection or a Protocol-installed OAuth provider, mirroring
  the agent-config revision owner that already governs it (`ConfigScopeAgent`).
  It is used for exactly one thing beyond that revision ownership: scoping the
  run-start reconcile VIEW so a run only touches ITS OWN owner's runtime-added
  entries — never boot-declared servers, never another owner's adds. It is NOT an
  isolation principal (§6: `agent_id` is not an isolation key) and is never used
  for dispatch or a storage `WHERE` clause; boot infra + the bare-name tool
  catalog stay process-global and deployment-shared (D-287, preserved). A shared
  runtime therefore trusts co-tenant admins for runtime-added connection names (a
  collision fails loud); hard isolation of runtime-added tools is a
  one-runtime-per-tenant deployment. Phase 167, D-301 (extends D-287).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] **If multi-isolation code paths changed:** N/A for hard isolation — this
      phase adds an owner-scoped reconcile VIEW, not an isolation key; the
      bounded guarantee is documented and NO false isolation property is
      asserted (the review verifies this)
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent reconciles across ≥2
      owners + boot reads against ONE shared `Registry` under `-race`; owner
      scoping holds; no torn state; no goroutine leak (D-025)
- [ ] **Integration test exists**
      (`test/integration/phase167_owner_scoped_reconcile_test.go`), wires real
      drivers end-to-end with a boot server + two owners, asserts owner-scoped
      reconcile + boot visibility, covers ≥1 failure mode (loud name collision),
      runs under `-race`
- [ ] D-287 preservation proven (the existing detach + shared-catalog tests pass
      unmodified; the two NOTEs REWRITTEN, not deleted)
- [ ] §18 skill hygiene: internal reconcile scoping touches no operator-followed
      surface beyond the runtime-add Console copy (the bounded-guarantee note) —
      `observe-with-the-console` / `use-the-harbor-protocol` checked; noted in PR
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + D-301 records the
      departure from the triple-keying draft
