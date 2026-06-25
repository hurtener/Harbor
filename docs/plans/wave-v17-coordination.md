# Harbor v1.7 CANDIDATE band — wave coordination

> Per Harbor §17.7 wave delivery cadence. This is the coordination artifact
> for the v1.7 candidate band; it sequences the three authored phase plans
> (128 / 129 / 130) into a single staged wave, prescribes the drain-merge
> order, the wave-end E2E, and the §17.5 checkpoint audit that gates the
> next band.

## Band name

**Protocol-edge hardening: capability negotiation, key-revocation safety,
and data-lifecycle erasure.**

This band hardens three independent edges of the Harbor Protocol surface so
that **generic Protocol clients** — third-party Consoles, IDE/TUI clients,
and SDK consumers — and **operators** get honest, fail-loud, lifecycle-
complete behavior out of the box:

- **Capability negotiation** (128) — a client learns which conditionally
  mounted surfaces a runtime actually serves *at attach*, instead of
  method-probing and catching a `501`/`unknown_method`. This is the
  established Harbor discipline (`topology_snapshot`, `state_snapshots`,
  `runtime_posture`, `events_subscribe`) extended to the agent-config
  control plane. A negotiable surface is a stable contract; a probe-only
  surface is not.
- **Fail-loud security** (129) — the production JWKS validator can no longer
  honor a possibly-revoked signing key *forever* during a prolonged IdP
  outage. A configurable max-stale ceiling converts an unbounded silent-
  availability bias into an explicit, observable, fail-closed rejection.
- **Data lifecycle** (130) — the Protocol gains its first identity-scoped
  **erasure** verb. Any client can satisfy a right-to-erasure / data-
  lifecycle request through the canonical wire contract, with a real three-
  store cascade and a fail-loud refusal on a running session — no privileged
  internal back-door.

All three motivate purely as open-source framework work: capability
negotiation for generic clients, fail-loud key-revocation safety for any
external-issuer deployment, and a canonical data-lifecycle surface for any
operator. None depends on the others; they share the band because they are
the coherent "Protocol-edge hardening" slice next on the critical path.

## The three phases

| Phase | Title | Decision | ProtocolVersion | Wire change |
|-------|-------|----------|-----------------|-------------|
| 128 | Advertise the agent-config control plane as a Protocol capability | D-260 | 0.1.0 (held) | digest-only (new capability member) |
| 129 | JWKS max-stale / revocation ceiling | D-261 | 0.1.0 (held) | none (auth + config only) |
| 130 | Session erasure Protocol method (data-lifecycle deletion) | D-262 | 0.1.0 (held) | additive method + error code + capability + types |

### Phase 128 — agent-config capability (D-260)

- **Primitive:** the canonical capability constant
  `CapAgentConfig = "agent_config"` registered in the ONE home for Protocol
  capability constants (`internal/protocol/types/version.go`:
  `Capability` const block + `canonicalCapabilities`).
- **Consumer (same phase, §13):** the conditional `runtime.info`
  advertisement wired to the *actual* surface mount — `PostureDeps` gains
  `AgentConfigAvailable`, `wiredCapabilitiesFor` appends `CapAgentConfig`
  only when set, and the boot paths (`cmd/harbor/cmd_dev.go`,
  `harbortest/devstack`) flip the flag from the same source-of-truth boolean
  that decides `WithAgentConfigService`. Plus the conformance handshake
  assertion (canonical set 5 → 6) and the posture conditional-advertisement
  test. A runtime that mounts the surface advertises it; one that does not,
  does not.

### Phase 129 — JWKS max-stale ceiling (D-261)

- **Primitive:** a configurable max-stale ceiling on `auth.JWKSKeySet`
  (`WithJWKSMaxStale` + immutable `maxStale` + the `KeyByID` fail-closed gate
  returning a wrapped `ErrJWKSStale`), plus the `identity.jwks_max_stale`
  config field with fail-loud validation (negative / below-floor rejected;
  zero applies the safe default).
- **Consumer (same phase, §13):** the `Validator` surfaces the staleness
  reason distinctly — the keyfunc propagates `ErrJWKSStale` (not masked as
  `ErrUnknownKey`), `mapParserError` honors it first, and
  `middleware.reasonForWire` emits the `jwks_stale` wire reason. Proven by a
  controllable-clock test (no `time.Sleep`): fresh → stale-rejected →
  successful-refresh-resets → verifies again.

### Phase 130 — session erasure method (D-262)

- **Primitive:** the additive `sessions.delete` method + `session_running`
  (409) error code + `SessionsDeleteRequest`/`SessionsDeleteResponse` wire
  types + `CapSessionLifecycle` capability + the `StateStore.DeleteScope`
  cascade primitive (single mandatory interface, conformance parity across
  in-mem / SQLite / Postgres). All single-sourced.
- **Consumer (same phase, §13):** the real three-store cascade handler
  (refuse-if-running → artifacts → memory → state → session-record hard-
  delete → redacted `session.erased` event) over the production drivers,
  exercised end-to-end (delete → subsequent read `not_found`, cross-store
  erasure proven, cross-tenant 403, running-task 409) and by a live smoke
  client.

## Staging — a single stage of three parallel worktree agents

All three phases are **independent** — no inter-phase `Deps`. Each lists
only *already-shipped* prerequisites (128 → 58/72f/92a/127; 129 →
115/55/56/61; 130 → 08/11/17/18/58/60/61). None depends on a sibling in
this band. Therefore the band is **one stage**: three parallel worktree
agents, one phase per agent, one general-purpose agent each.

The §13 primitive-with-consumer rule is satisfied *within* each phase (each
ships its consumer in the same PR), so staging needs no extra consumer
phase.

### Shared single-source files (128 + 130) — drain-merge order

128 and 130 both touch the single-source Protocol files. If merged
concurrently they will three-way-conflict. The conflict surface:

| File | 128 touches | 130 touches |
|------|-------------|-------------|
| `internal/protocol/types/version.go` | adds `CapAgentConfig` to the `Capability` block + `canonicalCapabilities` | adds `CapSessionLifecycle` to the same block + registry |
| `internal/protocol/types/version_test.go` | `TestCapAgentConfig_Registered` | `TestCapSessionLifecycle_*` registration |
| `internal/protocol/conformance/conformance.go` | canonical-set count 5 → 6, `wantCaps` + `Accepts` | the same count assertion advances again (6 → 7) + its own cap |
| `internal/protocol/methods/methods.go` | — | adds `MethodSessionsDelete` + `canonicalMethods` + `IsSessionsMethod` (method-count assertion in `methods_test.go`) |
| `internal/protocol/singlesource/singlesource.go` | — | registers `SessionsDeleteRequest`/`SessionsDeleteResponse` in `CanonicalWireTypes` |
| `web/console/src/lib/protocol/wire-manifest.gen.json` | regenerated (digest shifts — new capability member) | regenerated (new method + code + capability + types) |
| `docs/site/protocol/{methods,errors,types}.md` | no-diff (capability-only) | regenerated (D-209) |
| `docs/decisions.md` | D-260 (pre-assigned) | D-262 (pre-assigned) |
| `docs/glossary.md` | "Agent-config capability" | "session erasure", "erasure cascade" |
| `docs/plans/README.md` | 128 row + detail block | 130 row + detail block |

**Drain-merge order: 128 BEFORE 130.** Both extend the same
`canonicalCapabilities` registry, the same conformance canonical-set *count*
assertion, the same `wire-manifest.gen.json` digest, and the same
`docs/decisions.md` / `glossary.md` / `README.md` tails. Land 128 first,
then 130 rebases onto it:

1. Merge **128**. Its conformance count lands at 6; its capability member
   lands in the registry; the manifest digest regenerates once.
2. **Rebase 130 on the merged 128** before merging. The rebase resolves
   trivially because the changes are *additive at the tail* of each shared
   list (another `Capability` const, another `canonicalCapabilities` entry,
   the count assertion advances 6 → 7, another `CanonicalWireTypes`
   registration, another manifest member). After the rebase, 130 re-runs
   `make protocol-ts-gen` (so the committed manifest reflects BOTH the new
   capability from 128 *and* 130's method/code/types) and
   `make protocol-docs-gen`, then re-runs `make protocol-ts-gen-check`
   + `make protocol-docs-gen-check` green before merge.
3. The pre-assigned D-numbers (D-260, D-261, D-262) and pre-assigned
   capability/method/code names guarantee no symbol collision; the only
   mechanical conflict is adjacency in the shared lists, which the
   128-before-130 order makes a clean append rather than a three-way merge.

**Method-count assertion note (130).** `internal/protocol/methods/methods_test.go`
asserts a fixed canonical method count. Only 130 adds a method, so only 130
advances that count — 128 does not touch `methods.go`. No conflict there
*between* the two phases, but the implementing agent for 130 must bump the
count assertion in lockstep with the `canonicalMethods` append.

### 129 is conflict-free

Phase 129 touches **auth + config only** (`internal/protocol/auth/*`,
`internal/config/*`, `examples/*.yaml`) plus its own decisions/glossary/
README tails. It makes **no wire change** — no new method, error `Code`,
wire type, or capability; `ProtocolVersion`, `wire-manifest.gen.json`, and
the generated docs are all untouched (its smoke script *asserts* they are
unchanged). It does not touch `version.go`, `methods.go`,
`conformance.go`, or `singlesource.go`. It can merge in any order relative
to 128/130 with zero conflict on the Protocol single-source files. The only
shared files are the append-only doc tails (`docs/decisions.md` D-261,
`docs/glossary.md`, `docs/plans/README.md`) — trivial appends resolved by
ordering, not content overlap.

**Recommended merge sequence for the whole stage:** 128 → 130 (rebased on
128) → 129 at any point (slot it last to keep the doc-tail appends linear,
or first since it never conflicts on code). The load-bearing constraint is
only **128 before 130**.

## Wave-end E2E (§17.7 step 5)

Bundle a `test/integration/waveN_test.go`-shaped suite with the final phase
merged in the stage (it must import the surfaces of all three, so it lands
after the last merge — practically, alongside or immediately after 130 since
130 is the last code merge). It is **in addition to** each phase's own
integration test (`phase128_*`, `jwks_max_stale_test`, `phase130_*`), which
remain the per-phase gates. The wave-end E2E proves the *combined* surface
is alive together:

- **Real drivers across the band's surface.** Assemble one `httptest.Server`
  with the real control transport, real State / Memory / Artifact production
  drivers, a real `Registry`, the real `agentcfgprotocol.Service`, and a real
  JWKS-backed `Validator` with an injected controllable clock.
- **Combined-surface assertions:**
  1. `runtime.info` over a verified triple advertises **both**
     `agent_config` (128) **and** `session_lifecycle` (130) when both
     surfaces are wired — proving the two capability additions coexist in
     one `capabilities` projection and the conformance universe is internally
     consistent.
  2. A full erasure lifecycle (130): open → state + turn + artifact +
     completed task → `sessions.delete` → assert deletion counts, then
     `sessions.inspect` → `not_found`, `state.history` → empty, artifact
     `List` → empty, memory `GetLLMContext` → clean.
- **≥1 failure mode** (mandatory, §17.3) — exercise at least one per edge:
  - **129 fail-closed:** advance the injected clock past the max-stale
    ceiling with the JWKS fetcher failing; a request that verified while
    fresh is now rejected `401` with wire `reason: "jwks_stale"`; a
    successful refresh resets staleness and it verifies again.
  - **130 fail-loud refusal:** a `sessions.delete` against a session with a
    RUNNING task is refused `409 session_running` with **all stores
    untouched**; a cross-tenant erasure without `auth.ScopeAdmin` is
    `403 scope_mismatch`.
- **Identity propagation** asserted end-to-end through every layer the test
  wires (the multi-isolation triple flows from the verified token through the
  handler into the scoped stores; cross-tenant isolation lives or dies here).
- **N≥10 concurrency stress** (§17.3, long-lived wiring) — concurrent
  `sessions.delete` of **distinct** sessions interleaved with `runtime.info`
  and JWKS `Validate` calls against the single shared assembly; assert no
  cross-talk (erasing A never touches B), no data race, no goroutine leak
  after teardown.
- **`-race` is the gate.** Runs on every supported OS; no `time.Sleep` as a
  sync primitive (use the injected clock + channel/`eventually` assertions).

## Wave-end checkpoint audit (§17.5) — gates the next band

After all three merge and the wave-end E2E is green, run the read-only §17.5
checkpoint audit **before** scoping the next band. The audit:

1. Reads each shipped phase's source + tests + plan + RFC reference (128,
   129, 130).
2. Hunts for: wiring gaps (esp. the **128 advertisement-vs-mount drift**
   hazard — confirm `runtime.info` cannot advertise `agent_config` /
   `session_lifecycle` without the surface actually mounted, in *both*
   `cmd/harbor` and `harbortest/devstack`, per §17.6 the production-parity
   rule), RFC drift, depth issues, weak tests, hygiene regressions
   (markdownlint on `docs/decisions.md`, no committed merge markers, no
   godoc-visible phase/`D-NNN`/brief refs in `internal/`/`cmd/` Go source).
3. Specifically re-checks the §17.6 cross-phase parity shapes this band is
   prone to:
   - 128: the posture flag is wired from the *same* boolean as the mount in
     both boot paths (not just devstack — the PR #121 / cmd_dev failure mode).
   - 130: `cmd/harbor/cmd_dev.go` and `harbortest/devstack` both assemble the
     eraser into the sessions `Service` (or `CapSessionLifecycle` is honestly
     absent where the eraser is absent); memory `Flush` leaves no residual
     across *every* driver (fix the driver, not just the fixture).
   - 129: no opt-out knob crept in; the ceiling is identity-agnostic and
     gates before a verified identity exists.
4. Produces a categorised punch list (FAIL / WARN / NIT) with file:line refs.
5. Lands as a single `chore(checkpoint): v1.7 band audit fixes` PR.

**This audit gates the next band's planning** — do not scope band N+1 until
this PR is merged.

## ProtocolVersion impact — held at 0.1.0

All three changes are **additive, Minor-class** surface additions per the
`internal/protocol/types/version.go` Major/Minor/Patch taxonomy; bumping
`ProtocolVersion` is an RFC change (RFC §5.3), which is precisely why none of
these does it:

- **128** — a new capability member (old clients ignore an unknown
  capability; new clients gate on it). Only the wire-surface digest in
  `wire-manifest.gen.json` moves (it hashes `types.Capabilities()`).
- **129** — no wire change at all; reuses `CodeAuthRejected` (401) and the
  existing free-form `reason` field; manifest and generated docs untouched.
- **130** — a new method + error code + capability + wire types, all
  additive (old clients neither call nor break); manifest + generated
  `methods.md`/`errors.md`/`types.md` regenerate (D-209) but
  `ProtocolVersion` holds.

The precedent is firm: the four capabilities added since 0.1.0
(`events_subscribe`, `runtime_posture`, `topology_snapshot`,
`state_snapshots`) each landed without a version bump. `ProtocolVersion`
stays `0.1.0` across this entire band.

## §16 placement ritual for each implementing agent

Every dispatched agent operates **only inside its own worktree** (`pwd`
first; STOP if a path resolves outside it). The authored plan for each phase
already carries a turnkey "Implementation handoff" section (the README rows,
the D-NNN decisions entry, the smoke assertions, the detail-block stub) —
the agent's §16 placement job is to land those artifacts into the real repo:

1. **Place the plan.** `cp` the authored plan from this band's directory into
   `docs/plans/phase-NN-slug.md` in the worktree
   (`phase-128-agent-config-capability.md`,
   `phase-129-jwks-max-stale-ceiling.md`,
   `phase-130-session-erasure-method.md`).
2. **Smoke skeleton.** `cp scripts/smoke/_template.sh scripts/smoke/phase-NN.sh
   && chmod +x` it, then add the real assertions verbatim from the plan's
   "Smoke script additions" / handoff (c) section. Each assertion maps 1:1 to
   an acceptance criterion; use `scripts/smoke/common.sh` helpers only (no new
   curl wrappers).
3. **README rows.** Append the phase's `docs/plans/README.md` index row AND
   per-phase detail block (handoff (a) + (d)). If no v1.7 band header exists
   yet, the first agent's `Pending (V1.7)` status introduces it; follow the
   existing column order exactly. Flip the row to `Shipped` on land
   (130 also adds a one-line pointer to `sessions.delete` in the root
   `README.md` Status prose at the release cut; 129 adds its README Status
   row on land; 128's root-README mention rides the V1.7 release cut, not the
   phase).
4. **Decisions entry.** Append the pre-assigned `D-NNN` block (D-260 / D-261
   / D-262) from handoff (b) at the end of `docs/decisions.md`,
   **markdownlint-clean** — blank lines around the `---` separator and around
   the `## D-NNN` heading. Run `markdownlint-cli2` repo-wide before
   committing (latent `docs/decisions.md` breakage surfaces one PR late
   because CI lints repo-wide).
5. **Glossary.** Add the phase's glossary term(s) in the same PR (128:
   "Agent-config capability"; 129: "JWKS max-stale ceiling"; 130: "session
   erasure" + "erasure cascade").
6. **§18 skill drift.** Grep `docs/skills/` for the surface the phase touches
   (`runtime.info`/capabilities for 128; `identity.` config for 129;
   `sessions.` for 130); update any SKILL.md that *quotes the shape* in the
   same PR, record exemptions in the PR body.
7. **Regen gates where applicable.** 128 + 130 run `make protocol-ts-gen`
   (manifest) and (130) `make protocol-docs-gen`; all three confirm
   `make protocol-ts-gen-check` + `make protocol-docs-gen-check` pass. 129
   asserts the manifest + `ProtocolVersion` are *unchanged*.
8. **Gate before commit.** `make drift-audit` (required headings, RFC
   `§N.M` + `brief NN` references resolve, smoke script exists, mirror
   invariant, forbidden-name scan) AND `make preflight` (drift-audit + every
   phase smoke against the live build) must both pass. Commit only when both
   are green.

**Godoc-visible-source discipline (binding, §13).** No internal phase
numbers (`Phase NN`/`phase-NN`), inline `D-NNN`, `brief NN`, or wave-band
references in non-test Go source under `internal/` or `cmd/`. Name the
FEATURE, not the number. The authored plans' "Public API surface" godoc
already follows this — preserve it verbatim. Plan prose, decisions entries,
and `_test.go` files may reference numbers freely.

## Dispatch prompt checklist (§17.7 step 3)

Each of the three dispatch prompts MUST include: the master-plan detail
block (from handoff (d)); the mandatory reading list (relevant CLAUDE.md
sections, the cited RFC sections, the predecessor phase plans named in
`Deps`, the informing briefs); the §16 phase-plan workflow; the validation
gate (`make drift-audit` + `make preflight`); the **pre-assigned D-number**
(D-260 / D-261 / D-262); the **workspace warning** (operate only inside the
worktree; `pwd` first; STOP if a path resolves outside it); the
**markdownlint hygiene reminder** (blank lines around `---` and `## D-NNN`
in `docs/decisions.md`; run `markdownlint-cli2` repo-wide before committing);
and — for 128 and 130 — the **drain-merge note** (128 before 130; 130
rebases on merged 128 and re-runs `make protocol-ts-gen` +
`make protocol-docs-gen` after the rebase).
