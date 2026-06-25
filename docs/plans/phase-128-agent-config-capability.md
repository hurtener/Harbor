# Phase 128 — Advertise the agent-config control plane as a Protocol capability

## Summary

Make the agent-config control plane (`agent_config.*`, including the
session-safe and user tiers) **discoverable at connect-time by any
Protocol client** instead of forcing a method-probe. Today the surface is
mounted conditionally (`transports.WithAgentConfigService` — when not
supplied, the `/v1/agent_config/*` routes are simply absent), yet
`runtime.info.capabilities` advertises nothing for it: a third-party
Console, an IDE/TUI client, or an SDK consumer can only learn whether the
runtime speaks the agent-config surface by firing a real call and
catching the `501`/`unknown_method` (or a transport 404) — clumsy, racy,
and a wasted round-trip. This phase adds a single canonical capability
constant — `CapAgentConfig = "agent_config"` — to the registered
capability universe (the PRIMITIVE), and wires `runtime.info` to advertise
it **iff the agent-config surface is actually mounted** (the CONSUMER,
landing in the same phase per CLAUDE.md §13), so a client gates the
agent-config surfaces on `caps.has('agent_config')` exactly as it already
gates `topology_snapshot`. No new method, no new error code, no new wire
type, no `ProtocolVersion` bump — the capability is a purely additive,
Minor-class surface addition that mirrors how `events_subscribe`,
`runtime_posture`, `topology_snapshot`, and `state_snapshots` each landed.

## RFC anchor

<!-- The drift-audit verifies every RFC §N.M reference resolves to a real heading. -->

- RFC §5.2 — What the Protocol exposes. The capability set is how a
  Runtime tells a client *which* of the §5.2 surfaces are live; this phase
  adds the agent-config control plane (the RFC §6.16 `version_hash`
  content surface, exposed as `agent_config.*`) to that negotiated set.
- RFC §5.3 — Versioning. Cited for exactly one rule: **bumping
  `ProtocolVersion` is an RFC change.** The additive-vs-breaking *change
  taxonomy* this plan reasons with (a new capability is a Minor-class,
  backward-compatible surface addition that needs no version bump) lives
  in `internal/protocol/types/version.go` — the `Version` struct's
  Major/Minor/Patch godoc (`Minor` is "bumped on a backward-compatible
  surface addition (a new method, a new capability, a new optional wire
  field)"). That is the source for "a new capability is backward-
  compatible," and it is why this phase needs no version bump.
- RFC §6.16 — Agent Registry. The agent-config control plane versions an
  agent's config (prompt layers, tool/MCP exposure, per-tool policy,
  skills) — the `version_hash` content surface of the registration
  identity. This phase makes that control plane's *presence* observable to
  a Protocol client via capability negotiation.

## Briefs informing this phase

<!-- The drift-audit verifies every `brief NN` resolves to docs/research/NN-*.md. -->

- brief 06
- brief 11

## Brief findings incorporated

- brief 06 §1: *"the event bus the single contract that has to stay
  stable across versions — and unlocks remote attach, multi-runtime fleet
  view, IDE/TUI integrations, and observability-vendor adapters as natural
  extensions rather than custom features."* — capability negotiation is
  how a generic client (not just Harbor's own Console) learns the surface
  mechanically; a surface a client must *probe* to find is not a stable
  negotiable contract.
- brief 06 §9 Q-4: *"Best-effort additive (new `EventType`s and new
  optional fields are non-breaking)? … The latter is heavier but matters
  once third-party Consoles exist."* — a new capability is exactly the
  lightweight additive primitive that makes the "once third-party consoles
  exist" world work: an old client ignores an unknown capability, a new
  client gates on it, and no version bump is required.
- brief 11 (Console gating posture): *"the Console enforces UI gates *and*
  the Protocol enforces server-side; the UI gates are convenience (don't
  show buttons that would 403), not security."* — a client that wants to
  hide or disable the agent-config control panel when the runtime does not
  host it needs a positive signal at attach; "don't show buttons that
  would 404/501" requires knowing the surface is absent *before* clicking,
  which is precisely what a capability provides.
- brief 11 (admin-gated agent management): per-feature gates such as agent
  management require the admin scope — but scope-gating answers "may *I*
  use it," not "does this runtime *have* it." The two are orthogonal: a
  capability answers presence, the scope answers authorization. This phase
  adds only the presence signal; the existing admin-scope gate on the
  `agent_config.*` handler is unchanged.

## Findings I'm departing from (if any)

None.

## Goals

- A single canonical capability constant `CapAgentConfig = "agent_config"`
  registered in the ONE home for Protocol capability constants
  (`internal/protocol/types/version.go`: the `Capability` const block +
  the `canonicalCapabilities` registry) — no second definition site, no
  registration escape hatch (mirrors `CapTopologySnapshot` /
  `CapStateSnapshots`).
- `runtime.info.capabilities` advertises `agent_config` **iff this runtime
  instance actually mounted the agent-config surface** — a per-instance
  *wired-subset* projection driven by the same boolean that decides
  `transports.WithAgentConfigService`, so the advertisement can never claim
  a surface the runtime does not serve (the `topology_snapshot`
  conditional-advertisement pattern, applied to agent-config).
- The capability is in the canonical *universe* (so
  `types.Capabilities()` / `CurrentHandshake()` enumerate it and the
  version handshake negotiates against it), while the per-instance
  `runtime.info` advertisement stays conditional — the existing
  universe-vs-wired-subset distinction, preserved.
- A real CONSUMER in the same phase (CLAUDE.md §13): the boot paths that
  mount the agent-config surface (`cmd/harbor/cmd_dev.go` and
  `harbortest/devstack`) also flip the posture flag, and tests assert a
  runtime *with* the surface advertises `agent_config` on `runtime.info`
  while a runtime *without* it does not — `VersionHandshake` built from the
  reported `runtime.info` `Accepts(CapAgentConfig)` exactly when the
  surface is mounted.
- Fully additive: NO new method, NO new error code, NO new wire type, NO
  `ProtocolVersion` bump. The only generated/committed artifact that moves
  is the wire-surface digest in `wire-manifest.gen.json` (the digest hashes
  `types.Capabilities()`, which gains one member) — regenerated via
  `make protocol-ts-gen` and pinned by the existing lockstep gate.
- Fail-loud + identity-mandatory preserved end-to-end: the capability is
  build-global, identity-agnostic posture data returned only after
  `runtime.info`'s existing identity-edge gate (`CodeIdentityRequired` on
  an incomplete triple); nothing about this phase weakens that.

## Non-goals

- **A new Protocol method, error code, or wire type.** The capability
  rides the existing `runtime.info.capabilities` slice
  (`[]types.Capability`), which already carries the per-instance wired
  subset. Adding a method/handler/route for "does this runtime have
  agent-config?" would be strictly more surface for no value (see Risks).
- **Capabilities for `sessions.*` and `artifacts.*` in this phase.**
  Evaluated and DEFERRED — each is its own conditional-mount story with its
  own consumer wiring and tests, and bundling them dilutes the focus of
  this phase. They are not trivial (they are not always-on and not
  always-off; each needs a per-instance flag threaded through its own boot
  path). Recorded in Risks as a scoped follow-up, not a silent drop.
- **A Console consumer that gates the agent-config control panel on the
  capability.** The §13 consumer for *this* phase is the runtime-side
  conditional advertisement wired to the actual surface mount plus the
  conformance/handshake test — that is what exercises the primitive
  end-to-end. Rewiring the Console agent-config panel to gate on
  `caps.has('agent_config')` (it currently method-probes) is a clean
  follow-up Console phase under the §13 "no Console page without its
  feeding Protocol surface" rule — the feeding surface is exactly what this
  phase ships. Out of scope here; noted in Risks.
- **Sub-capabilities for the agent-config tiers** (`agent_config.session.*`
  vs `agent_config.user.*` vs the admin verbs). One capability advertises
  the *cluster's presence*; the three tiers are differentiated by SCOPE
  (`auth.ScopeAdmin` / `auth.ScopeAgentConfigUser` / session-safe), which
  the handler already enforces and which a client reads from its own JWT —
  not by capability. A finer-grained capability split is unwarranted and
  would re-introduce the optional-capability ceremony §4.4 forbids.
- **Bumping `ProtocolVersion`.** Additive Minor-class change; the four
  capabilities added since 0.1.0 set the precedent (none bumped the
  version). RFC §5.3's "bumping is an RFC change" rule is exactly why this
  stays additive.

## Acceptance criteria

<!-- Binding, testable. -->

- [ ] `internal/protocol/types/version.go` declares
      `CapAgentConfig Capability = "agent_config"` in the `Capability`
      const block AND registers it in `canonicalCapabilities`. The godoc
      names the FEATURE (the agent-config control plane), not any internal
      phase number / D-NNN / brief reference (CLAUDE.md §13 godoc rule).
      `types.IsValidCapability(types.CapAgentConfig)` is `true` and
      `types.Capabilities()` includes it (deterministically sorted).
- [ ] `types.CurrentHandshake().Accepts(types.CapAgentConfig)` is `true` —
      the canonical capability *universe* (what the Protocol version
      negotiates against) includes agent-config unconditionally, exactly as
      it includes `topology_snapshot`.
- [ ] `protocol.PostureDeps` gains an `AgentConfigAvailable bool` field
      (godoc'd: "this Runtime mounted the agent-config control plane; when
      true, `runtime.info.capabilities` advertises `agent_config` so a
      Protocol client gates the surface at attach"). `wiredCapabilitiesFor`
      gains the corresponding parameter and appends `CapAgentConfig` to the
      per-instance wired subset only when the flag is set;
      `NewPostureSurface` passes `deps.AgentConfigAvailable` through. The
      wired set stays lexicographically sorted.
- [ ] `runtime.info.capabilities` (the `handleInfo` projection) contains
      `agent_config` when `AgentConfigAvailable` is true and DOES NOT
      contain it when false — proven by a posture unit test that builds the
      surface both ways and asserts presence/absence (mirrors the existing
      `topology_snapshot` conditional-advertisement test).
- [ ] The CONSUMER is wired to the ACTUAL surface mount: in
      `cmd/harbor/cmd_dev.go` and `harbortest/devstack`, the boolean that
      decides whether `transports.WithAgentConfigService(...)` is supplied
      ALSO sets `PostureDeps.AgentConfigAvailable`, derived from one
      source-of-truth variable (e.g. `agentConfigEnabled := agentConfigService != nil`)
      so the advertisement can never drift from the mount. A test (or the
      integration test below) asserts the production wiring advertises
      `agent_config`.
- [ ] `internal/protocol/conformance/conformance.go` `runVersionHandshake`
      is updated: the canonical-set count goes 5 → 6, `CapAgentConfig` is
      added to `wantCaps`, and an `h.Accepts(types.CapAgentConfig)`
      assertion is added (matching the existing `CapTopologySnapshot`
      handshake-universe assertion). The conformance suite's
      "every entry in `types.Capabilities()` is observed in the version
      handshake" invariant stays green.
- [ ] A `version_test.go` unit test (`TestCapAgentConfig_Registered`,
      mirroring `TestCapStateSnapshots_Registered`) pins: the wire string
      is `"agent_config"`, `IsValidCapability` is true, `Capabilities()`
      includes it, and `CurrentHandshake().Accepts(CapAgentConfig)` is
      true.
- [ ] An integration test wires the REAL agent-config surface
      (`transports.WithAgentConfigService` with a real
      `agentcfgprotocol.Service`) AND the REAL `PostureSurface` behind an
      `httptest.Server`, calls `runtime.info` with a complete identity
      triple, and asserts the reported `capabilities` include
      `agent_config`; a sibling sub-test builds the mux WITHOUT the
      agent-config service and asserts `runtime.info` does NOT advertise
      `agent_config` (and the `/v1/agent_config/*` route is genuinely
      absent — `skip_if_404`-shaped). Covers ≥1 failure mode (an
      incomplete-identity `runtime.info` is rejected `401` /
      `identity_required` before any capability list is returned). Runs
      under `-race`.
- [ ] The committed `web/console/src/lib/protocol/wire-manifest.gen.json`
      is regenerated via `make protocol-ts-gen`: its top-level
      `wire_surface_digest` changes (the digest hashes
      `types.Capabilities()`, now six members), and
      `make protocol-ts-gen-check` passes (the `git diff` half is clean on
      the regenerated tree, the Go lockstep test
      `Manifest.WireSurfaceDigest == wiresurface.Digest()` passes, and the
      `.mjs` field-scan passes — no wire-type FIELD changed, so the TS
      `RuntimeInfo` / `Capability` interfaces are untouched).
- [ ] `make protocol-docs-gen-check` passes: the generated Protocol
      reference (`docs/site/protocol/{methods,events,errors,types}.md`)
      does NOT enumerate capability constants, so a capability-only
      addition produces no diff there; the gate still runs (D-209
      hygiene) and any diff it *does* produce is committed in the same PR.
- [ ] The hand-written Protocol-track pages that illustrate the capability
      set (`docs/site/protocol/versioning-and-compatibility.md`,
      `docs/site/protocol/build-a-client.md`) are reviewed per CLAUDE.md
      §18: if either *exhaustively* enumerates the capability set it gains
      an `agent_config` entry; if it only shows an *illustrative*
      (non-exhaustive) sample it is exempt — the PR records which.
- [ ] `scripts/smoke/phase-128.sh` asserts (each maps 1:1 to a criterion):
      `CapAgentConfig`/`"agent_config"` declared in `version.go` and
      present in `canonicalCapabilities`; `AgentConfigAvailable` present in
      `PostureDeps` and `wiredCapabilitiesFor` appends `CapAgentConfig`;
      the conformance count is 6 and names `CapAgentConfig`;
      `make protocol-ts-gen-check` AND `make protocol-docs-gen-check` pass;
      the new Go tests pass under `-race`; and, when the live dev server
      exposes `runtime.info` AND mounts the agent-config surface, a
      `runtime.info` call advertises `agent_config` in `capabilities`
      (skips per the 404/405/501 convention when the route or a dev token
      is unavailable). FAIL = 0.

## Files added or changed

```text
internal/protocol/types/version.go            # CapAgentConfig const + canonicalCapabilities entry (single source)
internal/protocol/types/version_test.go       # TestCapAgentConfig_Registered (mirror of CapStateSnapshots)
internal/protocol/posture.go                  # PostureDeps.AgentConfigAvailable; wiredCapabilitiesFor(topology, agentConfig)
internal/protocol/posture_test.go             # agent_config advertised iff AgentConfigAvailable; absence when false
internal/protocol/conformance/conformance.go  # runVersionHandshake: 5->6, +CapAgentConfig, +Accepts assertion
cmd/harbor/cmd_dev.go                          # PostureDeps.AgentConfigAvailable wired from agentConfigService != nil (CONSUMER)
harbortest/devstack/devstack.go                # same single-boolean wiring on the devstack boot path (CONSUMER)
web/console/src/lib/protocol/wire-manifest.gen.json  # regenerated — wire_surface_digest changes — GENERATED, do not hand-edit
test/integration/phase128_agent_config_capability_test.go  # E2E: runtime.info advertises agent_config iff surface mounted; missing-identity 401
scripts/smoke/phase-128.sh
docs/plans/phase-128-agent-config-capability.md
docs/decisions.md          # D-260 (markdownlint-clean: blank lines around --- and the ## heading)
docs/glossary.md           # "Agent-config capability" term
docs/plans/README.md       # Phase 128 index row + per-phase detail block (Pending (V1.7))
docs/site/protocol/versioning-and-compatibility.md  # §18: add agent_config only IF this page exhaustively enumerates capabilities
docs/site/protocol/build-a-client.md                 # §18: same conditional review
docs/skills/use-the-harbor-protocol/SKILL.md         # §18: if it quotes runtime.info capabilities, add agent_config (else exempt — record)
```

No new top-level directory (AGENTS.md §3 unchanged). No new package: the
constant lands in the existing `internal/protocol/types`, the conditional
advertisement in the existing `internal/protocol` posture handler.

Confirm the exact `harbortest/devstack` filename and the cmd_dev wiring
site at implementation time (`grep -rn "WithAgentConfigService\|PostureDeps" cmd/harbor harbortest`);
both are named here from the current tree but the implementor verifies
before editing.

## Public API surface

```go
// internal/protocol/types — one additive capability constant.

// CapAgentConfig advertises the agent-config control plane — the
// versioned desired-state surface for an agent's prompt layers, tool/MCP
// exposure, per-tool policy, and skills (the agent's content surface). A
// Protocol client negotiates "does this Runtime host the agent-config
// surface?" via VersionHandshake.Accepts(CapAgentConfig) / the
// runtime.info capability list, instead of method-probing the
// agent_config.* verbs. Conditional per-instance: a Runtime advertises it
// only when the agent-config surface is mounted. Backward-compatible
// (a Minor-class surface addition) — no ProtocolVersion bump.
const CapAgentConfig Capability = "agent_config"
```

```go
// internal/protocol — PostureDeps gains one additive flag.
type PostureDeps struct {
    // … existing fields …
    // AgentConfigAvailable indicates this Runtime mounted the agent-config
    // control plane. When true, runtime.info.capabilities advertises
    // agent_config so a Protocol client gates the surface at attach rather
    // than probing. Optional — defaults false (a Runtime that does not
    // wire the agent-config service). MUST be set from the same condition
    // that decides whether the agent-config transport is mounted, so the
    // advertisement cannot claim an absent surface.
    AgentConfigAvailable bool
}
```

No new Protocol method, error code, wire type, or transport route. The
capability constant and the additive `PostureDeps` flag are the only Go
surface other phases depend on. The `runtime.info` wire shape is
unchanged (`Capabilities []Capability` already exists) — only its
*contents* gain a possible member.

## Test plan

- **Unit (Go):** `version_test.go` — `TestCapAgentConfig_Registered`
  (wire string, `IsValidCapability`, `Capabilities()` membership,
  `CurrentHandshake().Accepts`). `posture_test.go` — `handleInfo`
  advertises `agent_config` iff `AgentConfigAvailable`; the cap does not
  leak when the flag is false; the wired set stays sorted; a
  `VersionHandshake` constructed from the reported `runtime.info`
  `Accepts(CapAgentConfig)` exactly when wired. (Extends the existing
  topology conditional-advertisement test table — add an
  `agentConfig bool` axis.)
- **Integration:** `test/integration/phase128_agent_config_capability_test.go`
  — real `agentcfgprotocol.Service` + real `PostureSurface` behind
  `httptest.Server` over the real control transport; sub-test A (surface
  mounted): `runtime.info` with a full triple advertises `agent_config`;
  sub-test B (surface NOT mounted): `runtime.info` omits `agent_config`
  AND `POST /v1/agent_config/get` is route-absent (404/501); failure mode:
  an incomplete-triple `runtime.info` is rejected `401` /
  `identity_required` before any capability list is produced. Identity
  propagation asserted end-to-end. `-race`.
- **Conformance:** `internal/protocol/conformance` — `runVersionHandshake`
  updated to the six-capability canonical set including `CapAgentConfig`
  and its `Accepts` assertion; the suite's "every `types.Capabilities()`
  entry is observed in the handshake" invariant must stay green (a new
  capability without its handshake coverage fails the suite — that is the
  trip-wire that forced this update).
- **Concurrency / leak:** N/A for a new artifact — this phase adds no new
  reusable artifact. The existing `PostureSurface` concurrent test
  (`posture_concurrent_test.go`) already runs N≥100 concurrent
  `runtime.info` dispatches against a single shared surface under `-race`;
  it continues to pass unchanged (the new field is set once at
  construction and read-only thereafter, preserving the D-025
  compiled-artifact contract). Confirm the concurrent test still asserts a
  byte-identical capability projection.

## Smoke script additions

`scripts/smoke/phase-128.sh` (header `# PREFLIGHT_REQUIRES:` mixes
`static-only` greps + `unit-tests` + a `live-server` tail; classify as
`live-server` since the tail hits the booted server — the safe default).
Uses `scripts/smoke/common.sh` helpers; no new curl wrappers.

- Static: `internal/protocol/types/version.go` declares
  `CapAgentConfig` and the `"agent_config"` wire string, and
  `canonicalCapabilities` lists `CapAgentConfig`.
- Static: `internal/protocol/posture.go` declares `AgentConfigAvailable`
  and `wiredCapabilitiesFor` appends `CapAgentConfig`.
- Static: `internal/protocol/conformance/conformance.go` names
  `CapAgentConfig` and the count is `6` (guards the trip-wire stays
  updated).
- Build/test: `make protocol-ts-gen-check` passes (manifest digest
  regenerated + in lockstep) AND `make protocol-docs-gen-check` passes;
  `go test -race ./internal/protocol/... ./internal/protocol/conformance/...`
  and `go test -race -run TestE2E_Phase128 ./test/integration/...` pass.
- Single-source defence: no `Capability` constant string is redefined
  outside `internal/protocol/types/version.go`
  (`assert_grep_absent 'Capability = "agent_config"'` over `internal/protocol`
  excluding `types/version.go` — defence-in-depth over the single-source
  lint).
- Live (skips per 404/405/501): when the dev server exposes
  `POST /v1/control/runtime.info`, a `runtime.info` call with a dev
  identity returns a `capabilities` array that, when the agent-config
  route is mounted, contains an entry whose `name == "agent_config"`.
  Drive it with `assert_post_status 200` (POSTs the `{"identity":{…}}`
  body, SKIPs on 404/405/501), then `jq` the capabilities out and grep for
  `agent_config`. SKIP when the route 404s or no dev token is available
  (same posture as `phase-72f.sh` / `phase-127.sh`).

## Coverage target

- `internal/protocol/types`: no regression below the package's existing
  target (the new constant + test only adds coverage).
- `internal/protocol` (posture handler delta): no regression below the
  package's existing ≥ 85%.
- `internal/protocol/conformance`: no regression (assertion-only delta).

## Dependencies

- Phase 58 — `internal/protocol` single-source layout (the `Capability`
  const block + `canonicalCapabilities` registry this phase extends is the
  ONE home for capability constants).
- Phase 72f — the `PostureSurface` + `runtime.info` wire type +
  `CapRuntimePosture` + the `wiredCapabilitiesFor` conditional-
  advertisement pattern (`TopologyAvailable`) this phase mirrors for
  agent-config.
- Phase 92a — the agent-config control plane (`agent_config.*`,
  `agentcfgprotocol.Service`, `transports.WithAgentConfigService`) whose
  presence this capability advertises.
- Phase 127 — the wire-surface digest in `runtime.info` +
  `wire-manifest.gen.json` + the `make protocol-ts-gen-check` lockstep gate
  (D-223/D-259); this phase's capability addition changes
  `types.Capabilities()`, hence the digest, hence the manifest regen rides
  the established gate.

(Phase 60 — the `internal/protocol/transports/control` wire transport the
integration test drives `runtime.info` through — is a transitive prereq of
72f and not separately load-bearing here.)

## Risks / open questions

- **New method vs additive capability — RESOLVED to additive capability.**
  A dedicated `agent_config.available` / `protocol.surfaces` method would
  add a method+handler+route+TS-type for a yes/no the
  `runtime.info.capabilities` slice already carries. The capability path is
  strictly less surface, costs one fewer round-trip (the client already
  calls `runtime.info` at attach), and is the established Harbor pattern
  for every other surface. Recorded in D-260.
- **Advertisement-vs-mount drift — the one real hazard.** The capability
  advertisement (`PostureDeps.AgentConfigAvailable`, consumed by the
  `PostureSurface`) and the actual route mount
  (`transports.WithAgentConfigService`, consumed by `NewMux`) are wired at
  two different call sites in the boot path. If they drift, `runtime.info`
  lies about the wire — the exact failure the `topology_snapshot`
  comment warns against ("Advertising `topology_snapshot` here would lie
  about the wire"). MITIGATION (binding in the acceptance criteria): both
  derive from ONE source-of-truth boolean per boot path
  (`agentConfigEnabled := agentConfigService != nil`), and the integration
  test pins "advertised iff mounted" against the real wiring. A future
  hardening could fold both into a single `WithAgentConfig(service)` option
  that sets the posture flag too — out of scope here; noted.
- **`sessions.*` and `artifacts.*` capabilities — DEFERRED, not trivial.**
  Per the design directive, evaluated for inclusion and deferred. Neither
  is a free add: `artifacts.*` and `sessions.*` are mounted through their
  own transport options, each needs its own `PostureDeps` flag threaded
  from its own boot condition and its own integration test, and the
  conformance count would then need to track three new members at once —
  three independent advertisement-vs-mount drift surfaces. Bundling them
  would violate this phase's focus and the "one capability, one consumer,
  one test" discipline. They are clean siblings for a follow-up phase
  (one capability each, same pattern). Recorded as an open follow-up in
  D-260; cutting them here is a recorded scoping decision, not a silent
  drop.
- **No Console consumer in this phase — by design, not omission.** The
  §13 consumer this phase ships is the runtime-side conditional
  advertisement wired to the real surface mount + the conformance/handshake
  test (the primitive is exercised end-to-end). A Console phase that gates
  the agent-config control panel on `caps.has('agent_config')` (replacing
  its current method-probe) is the natural next consumer and lands under
  the §13 "no Console page without its feeding Protocol surface" rule —
  the feeding surface is what this phase delivers. Listed as a follow-up,
  not a gap.
- **Digest churn is expected and gated.** Adding a capability changes
  `types.Capabilities()`, which changes `wiresurface.Digest()`, which
  changes the committed manifest's `wire_surface_digest`. This is the
  intended signal (a connected client that vendored the old manifest sees
  a *coarse* drift signal at connect-time and re-vendors) and is fully
  handled by `make protocol-ts-gen` + the existing lockstep test pinning
  `Manifest.WireSurfaceDigest == wiresurface.Digest()`. No special
  handling beyond the regen.
- **No field-shape change ⇒ no TS interface edit.** `Capability` is a TS
  interface (`{ name: string; version?: string }`), not a string-literal
  union, so a new capability VALUE needs no `protocol.ts` /
  `settings.ts` edit; the `.mjs` field-scan is unaffected. Only the
  generated manifest's digest value moves. Confirm at implementation time
  that no TS code hard-codes a capability allow-list that would need
  `agent_config` (the Console gates via `caps.has(...)` on the runtime-
  reported set, so it does not).
- **§18 skill drift (binding).** Any skill that NAMES `runtime.info` /
  capabilities is reviewed (`grep -rl 'runtime.info\|capabilities' docs/skills/`);
  if a SKILL.md *quotes the capability list*, it gains `agent_config` in
  the same PR; if it only names the verb, it is exempt — the PR records
  which. `use-the-harbor-protocol` is the most likely match.
- **§18 docs-site nav.** No new skill/recipe/reference page is added, so
  `docs/site/.vitepress/config.ts` is unchanged; the generated Protocol
  reference regenerates via `make protocol-docs-gen` (expected no-diff for
  a capability-only addition).

## Glossary additions

- **Agent-config capability** — the Protocol capability string
  `agent_config` (`types.CapAgentConfig`) that advertises the presence of
  the agent-config control plane (`agent_config.*`) on a runtime. A
  Protocol client negotiates the surface via
  `VersionHandshake.Accepts(CapAgentConfig)` / the `runtime.info`
  capability list instead of method-probing the verbs. Advertised
  per-instance only when the agent-config surface is mounted; in the
  canonical capability universe unconditionally. Additive (no
  `ProtocolVersion` bump). Add to `docs/glossary.md` in the same PR.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes (no AGENTS.md/CLAUDE.md edits — N/A)
- [ ] `make protocol-ts-gen-check` passes (manifest digest regenerated +
      in lockstep; no TS field change)
- [ ] `make protocol-docs-gen-check` passes (D-209: no generated-docs diff
      for a capability-only add; commit any diff it produces)
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
      — N/A (the capability is build-global, identity-agnostic posture data
      returned only after `runtime.info`'s existing identity gate; no new
      identity-scoped storage path)
- [ ] **Concurrent-reuse test** — N/A: this phase adds no new reusable
      artifact; the additive `PostureDeps` flag is set once at construction
      and read-only thereafter (D-025 preserved). The existing
      `posture_concurrent_test.go` (N≥100 concurrent `runtime.info`) still
      passes.
- [ ] **Integration test exists, real drivers/transport end-to-end,
      identity propagation, ≥1 failure mode, `-race`.**
      `test/integration/phase128_agent_config_capability_test.go`.
- [ ] §18 skill check: `grep -rl 'runtime.info\|capabilities' docs/skills/`
      — any skill quoting the capability list updated in this PR; record
      which were exempt
- [ ] If new vocabulary: glossary updated (`Agent-config capability`)
- [ ] If a brief finding was departed from: justified + decisions.md entry
      — N/A, no departures; D-260 records the design decisions

---

## Implementation handoff

Turnkey artifacts for the implementing agent. Operate only inside your
worktree (`pwd` first; STOP if a path resolves outside it). Run
`markdownlint-cli2` repo-wide before committing (blank lines around `---`
and `## D-NNN` headings in `docs/decisions.md`).

### (a) Master-plan `docs/plans/README.md` index row

Append (the table is sorted by phase number; this row sorts after 127). If
no V1.7 band header exists yet, the `Pending (V1.7)` status introduces it;
follow the existing column order exactly (`| N | desc | subsystem | RFC |
deps | coverage | status |`):

```text
|128 | Advertise the agent-config control plane as a Protocol capability (add the canonical `CapAgentConfig = "agent_config"` constant to `canonicalCapabilities`; `runtime.info.capabilities` advertises it IFF the agent-config surface is mounted — the `topology_snapshot` conditional-advertisement pattern, wired to `WithAgentConfigService` via one source-of-truth boolean; lets a Protocol client gate the `agent_config.*` surfaces at attach instead of method-probing `501`/`unknown_method`; NO new method, NO error code, NO wire type, NO version bump; conformance handshake set 5→6; only the wire-surface digest regenerates; D-260) | internal/protocol/types + internal/protocol (additive) + cmd/harbor + harbortest/devstack + internal/protocol/conformance | §5.2, §5.3, §6.16 | 58, 72f, 92a, 127 | 85% | Pending (V1.7) |
```

The root `README.md` "## Status" section is release-cut prose (not a
per-phase table) — it gains a one-line mention of the agent-config
capability only at the V1.7 release cut, not per phase.

### (b) `docs/decisions.md` entry (markdownlint-clean — note the blank lines)

Append at end of file:

```markdown

---

## D-260 — Advertise the agent-config control plane as a Protocol capability (agent_config) via an additive runtime.info-conditional capability, not a new method

**Date:** 2026-06-25

**Status:** Accepted (planning)

**Context.** The agent-config control plane (`agent_config.*` — the
admin verbs, the session-safe subset, and the durable user tier) is
mounted CONDITIONALLY (`transports.WithAgentConfigService`; when not
supplied the `/v1/agent_config/*` routes are absent). But
`runtime.info.capabilities` advertises nothing for it, so a Protocol
client (a third-party Console, an IDE/TUI client, an SDK consumer) can
only discover the surface by firing a real call and catching the
`501`/`unknown_method` (or a transport 404) — a clumsy, racy, wasted
round-trip. Every other conditionally-mounted Protocol surface
(`topology_snapshot`) is negotiable via a capability; agent-config was
the gap.

**Decision.**

1. **One canonical capability constant.** `internal/protocol/types/version.go`
   gains `CapAgentConfig Capability = "agent_config"` in the `Capability`
   const block and a `canonicalCapabilities` entry — the ONE home for
   capability constants (no second definition site, no registration escape
   hatch). `types.Capabilities()` / `CurrentHandshake()` enumerate it
   unconditionally (the negotiable universe).
2. **Conditional per-instance advertisement, wired to the actual mount.**
   `PostureDeps` gains `AgentConfigAvailable bool`; `wiredCapabilitiesFor`
   appends `CapAgentConfig` only when set; `runtime.info.capabilities`
   advertises `agent_config` iff this runtime mounted the surface — the
   `topology_snapshot` conditional pattern. The boot paths
   (`cmd/harbor/cmd_dev.go`, `harbortest/devstack`) set the flag from the
   SAME source-of-truth boolean that decides `WithAgentConfigService`
   (`agentConfigService != nil`), so the advertisement can never claim an
   absent surface.
3. **The consumer lands in the same phase (CLAUDE.md §13).** The primitive
   (the capability constant) ships with its consumer (the runtime-side
   conditional advertisement wired to the real surface mount) and a
   conformance/handshake test asserting a runtime with the surface
   advertises `agent_config` and one without it does not. A Console phase
   that gates the agent-config control panel on `caps.has('agent_config')`
   (replacing its method-probe) is the natural next consumer, under the
   §13 "no Console page without its feeding Protocol surface" rule.
4. **No new method.** A dedicated `agent_config.available` method was
   rejected: `runtime.info.capabilities` already carries the per-instance
   wired subset, the client calls `runtime.info` at attach anyway, and a
   capability is strictly less surface and one fewer round-trip.
5. **`sessions.*` and `artifacts.*` capabilities deferred.** Evaluated and
   scoped out: each needs its own `PostureDeps` flag, boot wiring, and
   integration test (its own advertisement-vs-mount drift surface);
   bundling them would dilute focus. Clean siblings for a follow-up phase,
   same pattern. A recorded scoping decision, not a silent drop.
6. **No version bump.** A new capability is a Minor-class, backward-
   compatible surface addition per the `internal/protocol/types/version.go`
   Major/Minor/Patch taxonomy; the four capabilities added since 0.1.0
   (`events_subscribe`, `runtime_posture`, `topology_snapshot`,
   `state_snapshots`) set the precedent — none bumped `ProtocolVersion`.
   RFC §5.3's rule that *bumping the version is an RFC change* is precisely
   why this stays additive: `ProtocolVersion` holds at 0.1.0. The
   capability addition changes `types.Capabilities()`, hence
   `wiresurface.Digest()`, hence the committed `wire-manifest.gen.json`
   digest — regenerated via `make protocol-ts-gen` and pinned by the
   existing lockstep gate (D-223/D-259). The conformance handshake set
   goes 5→6.

**§4.3 deviations.** None. Additive, follows the established
conditional-capability pattern exactly.

**Cross-references.** D-259 (the wire-surface digest the manifest stamps,
which this capability addition shifts), D-223 (the lockstep gate),
D-209 (the generated-docs regen gate), D-234..D-237 (the agent-config
control plane this advertises), D-256/D-257 (the durable user tier under
that plane). RFC §5.2, §5.3, §6.16.
`internal/protocol/types/version.go` (additive-vs-breaking taxonomy +
`CapTopologySnapshot` precedent). brief 06, brief 11. Plan:
`docs/plans/phase-128-agent-config-capability.md`.
```

### (c) `scripts/smoke/phase-128.sh` assertions to add

Use `scripts/smoke/common.sh` helpers; no new curl wrappers
(`assert_post_status` for the live POST already exists). Each maps 1:1 to
an acceptance criterion.

```bash
# Static: the capability constant + registration (single source).
assert_grep_present 'CapAgentConfig Capability = "agent_config"' \
  "internal/protocol/types/version.go" \
  "phase 128: CapAgentConfig constant declared"
assert_grep_present 'CapAgentConfig:' \
  "internal/protocol/types/version.go" \
  "phase 128: CapAgentConfig registered in canonicalCapabilities"

# Static: the conditional-advertisement wiring (the consumer).
assert_grep_present 'AgentConfigAvailable' "internal/protocol/posture.go" \
  "phase 128: PostureDeps.AgentConfigAvailable present"
assert_grep_present 'CapAgentConfig' "internal/protocol/posture.go" \
  "phase 128: wiredCapabilitiesFor appends CapAgentConfig"

# Static: the conformance trip-wire stayed updated (6 + named).
assert_grep_present 'CapAgentConfig' \
  "internal/protocol/conformance/conformance.go" \
  "phase 128: conformance handshake names CapAgentConfig"
assert_grep_present '!= 6' \
  "internal/protocol/conformance/conformance.go" \
  "phase 128: conformance canonical-set count is 6"

# Single-source defence: the capability string is defined only in version.go.
assert_grep_absent 'Capability = "agent_config"' \
  "internal/protocol/posture.go" \
  "phase 128: agent_config capability not redefined outside types (single-source)"

# Build/test gates: manifest lockstep (digest regen) + generated-docs gate.
if make protocol-ts-gen-check >/dev/null 2>&1; then
  ok "phase 128: make protocol-ts-gen-check passes (manifest digest regenerated + in lockstep)"
else
  fail "phase 128: make protocol-ts-gen-check failed (run make protocol-ts-gen and commit the manifest)"
fi
if make protocol-docs-gen-check >/dev/null 2>&1; then
  ok "phase 128: make protocol-docs-gen-check passes (no generated-docs drift)"
else
  fail "phase 128: make protocol-docs-gen-check failed (run make protocol-docs-gen and commit any diff)"
fi
if go test -race ./internal/protocol/... ./internal/protocol/conformance/... >/dev/null 2>&1; then
  ok "phase 128: protocol + conformance tests pass under -race"
else
  fail "phase 128: protocol/conformance tests failed (go test -race ./internal/protocol/...)"
fi
if go test -race -run TestE2E_Phase128 ./test/integration/... >/dev/null 2>&1; then
  ok "phase 128: agent-config capability E2E passes under -race (advertised iff mounted; missing-identity 401)"
else
  fail "phase 128: capability E2E failed (go test -race -run TestE2E_Phase128 ./test/integration/...)"
fi

# Live (skips per 404/405/501): runtime.info advertises agent_config when
# the agent-config surface is mounted. Build the {"identity":{...}} body
# from the dev triple and POST it with assert_post_status (SKIPs on
# 404/405/501). On a 200, jq the capabilities out and grep for
# agent_config — ok when present (the dev server mounts the surface), skip
# when the route 404s or no dev token is available (same posture as
# phase-72f.sh / phase-127.sh).
#   body='{"identity":{"tenant":"...","user":"...","session":"..."}}'
#   assert_post_status 200 "$(api_url /v1/control/runtime.info)" "$body" \
#     "phase 128: live runtime.info answers 200"
#   then curl the body, jq '.capabilities[].name', grep -q '^agent_config$'.
```

### (d) Master-plan per-phase detail-block stub

Add under the detail section of `docs/plans/README.md` (house format —
mirror the 127/125 blocks):

```markdown
### Phase 128 — Advertise the agent-config control plane as a Protocol capability

- **Subsystem:** internal/protocol/types (the `CapAgentConfig` constant +
  registration) + internal/protocol (the additive `PostureDeps` flag +
  conditional `runtime.info` advertisement) + cmd/harbor + harbortest/devstack
  (the boot-path consumer wiring) + internal/protocol/conformance (the
  handshake set 5→6).
- **RFC:** §5.2 (capabilities are how a client negotiates the §5.2
  surfaces), §5.3 (cited only for "bumping the version is an RFC change";
  the additive-vs-breaking taxonomy is version.go), §6.16 (the agent-config
  control plane this advertises).
- **Deps:** 58 (single-source capability registry), 72f (PostureSurface /
  runtime.info / the `topology_snapshot` conditional-advertisement pattern),
  92a (the agent-config control plane), 127 (the wire-surface digest +
  lockstep gate the manifest regen rides).
- **What it delivers:** a canonical `CapAgentConfig = "agent_config"`
  capability; `runtime.info.capabilities` advertises it IFF the
  agent-config surface is mounted (wired to `WithAgentConfigService` via
  one source-of-truth boolean); a Protocol client gates the `agent_config.*`
  surfaces at attach instead of method-probing. NO new method, error code,
  wire type, or version bump; only the wire-surface digest regenerates.
- **Consumer (§13, same phase):** the runtime-side conditional
  advertisement wired to the real surface mount + the conformance/handshake
  test (advertised iff mounted). A Console capability-gate is the natural
  follow-up.
- **Decision:** D-260.
- **Status:** Pending (V1.7).
```
