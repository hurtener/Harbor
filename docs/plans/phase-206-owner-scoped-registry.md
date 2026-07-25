# Phase 206 — owner-scoped registry mutation + connection-descriptor validation on revision writes

## Summary

Closes two halves of one contract on the runtime-added MCP connection surface. (1) The live MCP registry's discovery-origin mutator becomes **owner-scoped**: it takes the caller's `(tenant, agent)` owner and replaces the allow-list only on a registration carrying that same tag, and the `agent_config.set_mcp_discovery_origins` verb's boot-declared guard is hoisted so it applies on every path rather than only when the caller's own revision does not declare the name. (2) `agent_config.set_revision` — the second door onto the same revision spine — validates the MCP **connection descriptors** it persists against the SAME shape rules `add_mcp_connection` enforces, rejecting the whole set loud with a sentinel-preserving error.

## RFC anchor

- RFC §6.4 — tool catalog and transports (the MCP driver, the connection descriptor, the registry the attach path registers into).
- RFC §6.16 — the Agent Registry (the agent registration identity the `(tenant, agent)` owner tag mirrors; `agent_id` is registration metadata, never an isolation key).

## Briefs informing this phase

- brief 09 — MCP OAuth (the agent-bound authorization model + the identity-mandatory posture the registry write inherits).
- brief 03 — tools + integrations (the one-tool-abstraction catalog + the bare-name resolution model this phase deliberately does not widen).
- brief 14 — MCP client/host compliance (the host-side connection lifecycle the registry projects).

## Brief findings incorporated

- **brief 09 §"What Harbor must add" item 3** ("Admin-scope authz on agent-bound flows. Only callers with admin scope on the agent's tenant can initiate / complete / revoke `ScopeAgent` flows."). The discovery-origin write is an agent-bound operation, so admin scope is necessary but not sufficient — this phase adds the missing half, that the operation lands on the CALLER'S agent-bound registration rather than on any registration sharing the bare name.
- **brief 09 §"What Harbor must add" item 4** ("Identity-mandatory enforcement … fail closed on missing components … `ScopeAgent` requires `(tenant, agent)`"). The applier fails closed when either owner component is missing rather than widening to an unscoped write — the same posture `MCPConnectionAttacher.Attach` already takes (`ErrRuntimeAddOwnerMissing`).
- **brief 09 §"What Harbor must add" item 7** (the D-025 concurrent-reuse contract: "no cross-identity bleed, no cross-agent bleed, no scope confusion … Test mandatory"). The registry gains an N≥128 concurrent two-owner test asserting the terminal live allow-list only ever holds the owning owner's origins.
- **brief 03 §5 "Sharp Edges Harbor Must Avoid"** ("Harbor picks one architecture and bakes the correction in" — no parallel modes). The full-payload door reuses `validateConnection`, the add door's existing validator, rather than growing a second descriptor validator.

## Findings I'm departing from (if any)

None. The bare-name process-global registry (brief 03's one-catalog model, settled in D-287 and re-affirmed in D-301) is preserved exactly: resolution, dispatch, and every read projection stay bare-name and deployment-shared. Only the WRITE is owner-scoped, which is the same shape D-301 already established for the reconcile VIEW.

## Goals

- The live discovery-origin mutator applies to the caller's own registration; a name registered to a different `(tenant, agent)` owner, or boot-declared, resolves to no registration of the caller's.
- The Protocol edge distinguishes the three outcomes honestly: an owner refusal is `CodeScopeMismatch` / 403 with the revision rolled back; a not-yet-attached own connection degrades to `applied_live: false`; a boot-declared name is `CodeInvalidRequest` / 400.
- The boot-declared guard is a property of the NAME — it fires whether or not the caller's own active revision declares a connection under it.
- Every MCP connection descriptor persisted through `agent_config.set_revision` satisfies the shape rules `add_mcp_connection` enforces; a malformed set is rejected whole, with nothing persisted.
- The persisted descriptor round-trips completely through the wire projection, including `oauth_discovery_allowed_origins`.

## Non-goals

- Re-keying the MCP registry or the tool catalog by identity. D-287 / D-301 settled that they stay process-global and bare-name; this phase does not re-litigate it.
- Owner-scoping the other bare-name registry mutators (`SetRawHTMLTrust`, `Deregister`, `RefreshDiscovery`, `Probe`). They are reported in "Risks / open questions" and left for a follow-up; touching them reaches into `internal/protocol/mcp.go`, which a concurrent phase owns.
- Tightening the stdio allowlist beyond `argv[0]` (see "Risks / open questions"). The allowlist itself now applies at BOTH doors; only its binary-vs-argument granularity is out of scope.
- Any wire-type, method, error-code, or event change. `ProtocolVersion` is unchanged; no D-223 / D-209 regeneration.

## Acceptance criteria

- [ ] `Registry.SetOAuthDiscoveryOrigins` takes an `auth.Owner` and returns `ErrServerNotFound` when the named registration is absent, boot-declared, or owned by a different `(tenant, agent)`; the owning caller succeeds.
- [ ] The live allow-list and the recorded requirement of a registration are unchanged by a non-owning caller's write.
- [ ] `MCPConnectionAttacher.SetOAuthDiscoveryOrigins` fails closed on an incomplete owner and surfaces an owner refusal as `ErrConnectionOwnerMismatch`, which the wire handler maps to `CodeScopeMismatch` / 403.
- [ ] An owner refusal rolls the just-written revision back — the call leaves no observable effect.
- [ ] `set_mcp_discovery_origins` refuses a boot-declared name on BOTH the declared and not-declared paths, records no revision, and never reaches the live registry.
- [ ] `ReconcileDiscoveryOrigins` passes the reconciling owner to the live apply.
- [ ] A ZERO owner resolves to no registration at all — it never matches a boot-declared (zero-owner) entry.
- [ ] `agent_config.set_revision` rejects a malformed connection descriptor with an `ErrInvalidConnection`-preserving error naming `connections.servers[i]`; the active revision is unchanged and no new revision is recorded.
- [ ] `agent_config.set_revision` applies the fail-closed stdio command allowlist: the SAME descriptor is refused with the SAME `ErrStdioNotAllowed` sentinel at both doors, an empty allowlist refuses every stdio descriptor, an allowlisted command lands, and a refusal persists nothing.
- [ ] A well-formed connection descriptor persists through `set_revision` and round-trips through `get` / `list_revisions` / `diff`, `oauth_discovery_allowed_origins` included, in its NORMALISED form (trimmed name / URL, de-duplicated origins).
- [ ] The production attacher and detacher satisfy their agent-config / projection seams by compile-time assertion, so a signature drift is a build failure rather than a silently unwired binding.
- [ ] `scripts/smoke/phase-206.sh` shows `OK ≥ 6`, `FAIL = 0` against a live preflight build, and FAILS (never SKIPs) when any of the five guards is removed.

## Files added or changed

```text
internal/tools/drivers/mcp/
  registry.go                                     # owner param + ownedEntry resolution
  registry_set_oauth_discovery_origins_test.go    # owner-scope, boot-declared, concurrent-owners
  registry_oauth_discovery_test.go                # fixture registers under an owner
internal/runtime/agentcfg/protocol/
  setdiscoveryorigins.go                          # applier seam owner params; boot guard hoisted; ErrConnectionOwnerMismatch
  setdiscoveryorigins_test.go
  addconnection.go                                # validateConnectionsSection (reuses validateConnection)
  service.go                                      # SetRevision descriptor validation; descriptor origins carried both ways
  setrevision_connections_test.go                 # NEW
internal/runtime/agentcfg/projection/
  projection.go                                   # DiscoveryOriginReconciler carries the owner
  reconcile_discovery_origins_test.go
internal/runtime/serve/
  mcp_attacher.go                                 # owner params, fail-closed owner guard, owner classification
  mcp_detacher.go                                 # owner param
  coverage_test.go
  siblings_matrix_test.go                         # seed connection -> http (the stdio gate now applies here)
internal/protocol/transports/stream/
  agentconfig_handler.go                          # ErrConnectionOwnerMismatch -> CodeScopeMismatch / 403
test/integration/
  phase206_owner_scoped_registry_test.go          # NEW
  phase168_discovery_allowance_test.go            # harness gains boot-declared names; owner-aware call sites
scripts/smoke/phase-206.sh                        # NEW
docs/plans/phase-206-owner-scoped-registry.md     # NEW
docs/plans/README.md, docs/decisions.md, docs/glossary.md
docs/skills/use-the-harbor-protocol/SKILL.md      # §18 same-PR surface update
```

## Public API surface

```go
// internal/tools/drivers/mcp
// A zero owner owns nothing; an unregistered / boot-declared / other-owner name
// all answer ErrServerNotFound.
func (r *Registry) SetOAuthDiscoveryOrigins(ctx context.Context, name string, owner auth.Owner, origins []string) (prev []string, err error)

// internal/runtime/agentcfg/protocol
type DiscoveryOriginApplier interface {
    SetOAuthDiscoveryOrigins(ctx context.Context, tenant, agentID, name string, origins []string) (prev []string, err error)
}
var ErrConnectionOwnerMismatch = errors.New(...)

// internal/runtime/agentcfg/projection
type DiscoveryOriginReconciler interface {
    AttachedSources(ctx context.Context, owner auth.Owner) []string
    SetOAuthDiscoveryOrigins(ctx context.Context, owner auth.Owner, name string, origins []string) (prev []string, err error)
}
```

`validateConnectionsSection` is package-internal to `internal/runtime/agentcfg/protocol` — the shape authority stays one function (`validateConnection`), reachable from both doors.

## Test plan

- **Unit:**
  - `TestRegistry_SetOAuthDiscoveryOrigins_OwnerScoped` — a non-owning owner is refused with `ErrServerNotFound` and the live allow-list is unchanged; the owning owner succeeds.
  - `TestRegistry_SetOAuthDiscoveryOrigins_BootDeclaredIsOwnerScopedOut` — a runtime owner's write never reaches a zero-owner (boot-declared) registration.
  - `TestRegistry_SetOAuthDiscoveryOrigins_ZeroOwnerOwnsNothing` — the zero owner (and a half owner) resolves to nothing, so an owner-less caller cannot land on boot state.
  - `TestSetRevision_Connections_StdioAllowlistGatesBothDoors` / `..._EmptyStdioAllowlistRefusesEveryStdio` / `..._PersistsNormalizedDescriptor`.
  - `TestSetMCPDiscoveryOrigins_BootDeclaredRejectedWhenAlsoDeclaredInRevision` — the declared path reaches the same refusal, applier never called.
  - `TestSetMCPDiscoveryOrigins_OwnerMismatchFailsLoudAndRollsBack` — loud, never a degrade; revision rolled back.
  - `TestSetMCPDiscoveryOrigins_PassesCallerOwnerToTheApplier` / `TestReconcile_PassesReconcilingOwnerToTheLiveApply` — the owner reaches the live seam from both triggers.
  - `TestSetRevision_Connections_*` — 16 malformed shapes rejected with nothing persisted; whole-set rejection naming the offending index; an already-active revision unchanged by a rejected write; a valid descriptor round-tripping through get / list / diff; a nil section accepted.
  - `TestMCPConnectionAttacher_SetOAuthDiscoveryOrigins_IncompleteOwner` — fail-closed owner guard.
- **Integration:** `test/integration/phase206_owner_scoped_registry_test.go` — real StateStore-backed agent-config registry, real in-memory bus + real audit redactor, the real process-global MCP registry, the production `serve.MCPConnectionAttacher`, and the real Protocol service + wire handler. Covers cross-owner refusal (403 `scope_mismatch`) with the other owner's live allow-list intact and the caller's revision rolled back to the exact pre-write revision id and origin values; the boot-declared refusal on both paths; the `set_revision` descriptor rejection + valid round-trip; and the missing-identity failure mode. `TestE2E_OwnerScopedRegistryWrite_IsTheAuthoritativeEnforcement` drives the REAL registry directly rather than through the applier, so the registry layer's own scoping is pinned end to end and not only at the Protocol edge — including the ordering case the classification cannot cover (a registration superseded by another owner between the classification read and the write still refuses).
- **Conformance:** N/A — no new driver interface; the MCP registry is a single process-local concrete.
- **Concurrency / leak:** `TestRegistry_SetOAuthDiscoveryOrigins_ConcurrentOwners` (N=128 per owner against one shared registry under `-race`, terminal allow-list holds only the owning owner's origins) plus the pre-existing `..._ConcurrentReuse` / `..._NoPointerRace` suites, and `TestE2E_OwnerScopedDiscoveryWrite_ConcurrentCrossOwner` (N=16 concurrent own + cross-owner writers through the real wire handler).

## Smoke script additions

`scripts/smoke/phase-206.sh` (`live-server`):

- Live: `agent_config.set_revision` returns 400 for each of five malformed connection descriptors (stdio+url, http+command, http without url, empty name, unknown transport).
- Live: `agent_config.get` reports `set: false` after those rejections — nothing persisted.
- Live: `agent_config.set_revision` returns 403 for an un-allowlisted stdio command (the dev config declares no allowlist, so the fail-closed default refuses every stdio descriptor).
- Live: a well-formed descriptor returns 200 and its `oauth_discovery_allowed_origins` round-trips through `agent_config.get`.
- Live: `agent_config.set_mcp_discovery_origins` against an undeclared name stays a typed 404.
- Guard tests: three `go test -race -run` legs (registry owner scope incl. the zero-owner case; agentcfg boot-guard + owner refusal + descriptor validation + stdio gate; the integration seam covering both the Protocol edge and the registry layer). Each FAILS on a genuine failure and only SKIPs when the filter matches no tests at all.
- The agent id is per-invocation (`$$` + timestamp), so a second run against a long-lived server cannot read the first run's persisted write — verified by running the script twice against one server (OK: 9 both times).

Verified empirically — each of the six guards turns a corresponding `OK` into a `FAIL` when removed: the registry owner comparison; the registry zero-owner guard; the attacher's owner classification; the hoisted boot-declared guard; the `set_revision` descriptor validation; and the `set_revision` stdio allowlist.

## Coverage target

- `internal/tools/drivers/mcp`: 80% (unchanged target; the new branch is covered by three tests).
- `internal/runtime/agentcfg/protocol`: 80%.
- `internal/runtime/serve`: 70% (the attacher's new owner guard is covered).
- `internal/runtime/agentcfg/projection`: 80%.

## Dependencies

- 167 (D-301 — the owner tag + the owner-scoped reconcile view this write scope extends).
- 168 (D-302 — the discovery-allowance write + `Registry.SetOAuthDiscoveryOrigins` this phase re-scopes).
- 169 (D-303 — the `ProviderInstaller` `(tenant, agentID)` seam shape the applier seam mirrors).
- 92f / 203 (the `add_mcp_connection` door whose `validateConnection` the full-payload door now reuses).

## Risks / open questions

- **The stdio allowlist is a binary policy (`argv[0]`)** (`internal/runtime/agentcfg/protocol/addconnection.go:200-204` at the add door and `Service.gateStdioConnectionCommands` at the full-payload door; the allowlist is built in `internal/runtime/serve/mcp_attacher.go:234-239` from `tools.mcp_add_connection.stdio_allowlist`). Allowlisting a general-purpose launcher such as `npx` or `docker` therefore admits caller-chosen arguments under that one allowlisted binary. Whether it should also be an argument policy is an open design question, deliberately out of scope here — recorded as a follow-up.
- **Other bare-name registry mutators keep the unscoped shape.** `Registry.SetRawHTMLTrust` (`internal/tools/drivers/mcp/registry.go:983`, reachable over the Protocol via `mcp.servers.set_raw_html_trust`, `internal/protocol/mcp.go:885`), `Registry.Deregister` (`registry.go:431`), `Registry.RefreshDiscovery` (`registry.go:875`) and `Registry.Probe` (`registry.go:917`) all resolve by bare name with identity mandatory for authorization only. `Deregister`'s two production callers are already owner-guarded (`attach.go:246` via `OwnerOf`; the detach leg via `RuntimeAddedSources`), so the owner scope holds on every live path; the mutator shape is a separate follow-up. Reported, deliberately not fixed here: closing them reaches into `internal/protocol/mcp.go`, which a concurrent phase owns.
- **`agent_config.set_revision` is a second door onto the discovery-origin allow-list**, and it records the attributed revision event rather than the `mcp.discovery_origins_set` granted/revoked delta the dedicated verb emits. The live effect stays owner-scoped and boot-excluded either way (the run-start reconcile re-derives from the current revision), so this is an audit *granularity* difference between the two doors, not a difference in what can be reached — recorded as a follow-up.
- **`ErrRuntimeAddOwnerMissing` maps through the handler's `default:` arm** (500 rather than a 4xx). It is dead-defensive: `identityFromScope` rejects an empty tenant or agent id before the applier is reached, so no wire request can produce it. Left unmapped rather than expanding this phase's footprint in a concurrently-owned file.
- **The wire-handler error mapping is a one-case addition inside `internal/protocol/transports/stream/agentconfig_handler.go`**, a directory a concurrent phase is refactoring. The addition sits in `writeServiceError`'s sentinel switch, well away from the identity-reconciliation region, but it is the one file this phase touches outside its own subsystem — flagged for the merge.
- **Duplicate connection names inside one `connections.servers[]` set are not rejected.** `validateConnection` has no such rule, so adding one here would exceed "the same shape rules the add door enforces". A duplicate resolves last-write-wins through `findConnection`; a follow-up may tighten both doors together.

## Glossary additions

- **Owner-scoped discovery-origin write** — added to `docs/glossary.md` in this PR.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes (smoke verified against the live surface; see the report for the run mode)
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes
- [x] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. `TestRegistry_SetOAuthDiscoveryOrigins_ConcurrentOwners` runs N=128 per owner against one shared `Registry`.
- [x] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md §17. `test/integration/phase206_owner_scoped_registry_test.go`.
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: justified above + decisions.md entry filed (none departed)
