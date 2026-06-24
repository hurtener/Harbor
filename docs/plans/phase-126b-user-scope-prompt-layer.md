# Phase 126b — USER-scope durable prompt-layer projection

## Summary

Make the durable USER-scope prompt layer actually shape a run. Phase 126a
already PERSISTS a per-user `user_prompt` — it is one field of the
`AgentConfigUserPayload` the one durable user-scope write verb
(`agent_config.user.set_revision`) writes into a versioned, user-keyed config
revision. But nothing reads that field back at run start, so today it is
inert: a user can store a standing personal instruction and it never reaches
the LLM. This phase is the **run-start projection consumer** that closes that
gap — it is the consumer half of Harbor's "no primitive without a consumer"
rule for 126a's durable user write surface.

This phase is **PROJECTION-ONLY**. It adds NO store, NO write verb, NO method
constant, and NO wire type. The durable `user_prompt` is read back through
126a's existing registry read (`Registry.Active(..., ConfigScopeUser)`) and
threaded into the EXISTING lower-trust `<user_instructions>` composition as a
new MIDDLE segment, between the admin User layer and the ephemeral per-session
User overlay. A second user-keyed store + a second user-prompt write verb
would be a §13 "two parallel implementations of the same conceptual feature":
two writers feeding one reader, with a guaranteed drift bug the day they
disagree. There is exactly one writer (126a's `set_revision`) and exactly one
reader (this projection).

The only new surface is internal: the run-start projection wiring plus the
`composeUserLayer` signature/extension from two ordered segments to three. The
precedence becomes **admin Base > admin User > USER-durable > session User**,
all below the always-present admin-base spine, reusing the existing
composition + escaping machinery — no new prompt block, no new escaping path,
no new auth gate (the write already passed through 126a's user-scope tier).

## RFC anchor

- RFC §6.16 — Agent Registry (the agent's config — including the durable
  per-user revision 126a persists — is read back at run start under the
  caller's verified identity; `agent_id` is a registration key in the session
  slot, never an isolation principal, so the projection never widens the
  isolation tuple `(tenant, user, session)`).
- RFC §5.5 — Authentication (the verified identity triple keys the read; the
  durable layer was already gated at WRITE time by 126a's user-scope tier, so
  this read-side projection adds no new authority surface — it consumes the
  already-authorised durable revision).

## Briefs informing this phase

- brief 13
- brief 11

## Brief findings incorporated

- **brief 13 §2.1 + §2.3 (the prompt's trust gradient).** Brief 13 codifies
  that Harbor's ReAct system prompt has a fixed trust gradient: the
  operator-authored base is the always-present high-trust spine, and any
  text NOT authored by the operator (caller-supplied instructions, retrieved
  memory) lands in a dedicated LOWER-TRUST position — brief 13 §2.3 wraps
  memory blobs in explicit `UNTRUSTED` framing precisely so caller-sourced
  content can extend, but never replace or precede, operator guidance. The
  durable USER-scope layer is caller-authored, so it inherits exactly that
  placement: this phase composes it into the EXISTING lower-trust
  `<user_instructions>` block (the same block the admin User layer and the
  session overlay already share), never as a new high-trust section and never
  above the operator base. No new escaping/framing path is introduced — the
  durable layer rides the same builder escaping the session overlay already
  proves correct, so the trust gradient brief 13 establishes is preserved by
  construction. The composition ORDER is the security boundary: a caller
  layer can append to the operator's standing instruction, never outrank it.
- **brief 11 §"Architectural ground rules" rule 8** (*"The Console NEVER reads
  internal Runtime types. All data flows through canonical Protocol events /
  state snapshots … if the data isn't on the Protocol, the feature can't ship,
  and the Protocol-side phase needs to add it."*): the durable user layer's
  WRITE + READBACK surface is 126a's Protocol verb family — a generic Protocol
  client (a third-party console, an IDE client, the SDK) writes it through
  `agent_config.user.set_revision` and reads it back through `user/get`. This
  phase adds NO Console-private hook; it only makes the already-on-the-Protocol
  durable field shape a run. The feature is honest as a Protocol consumer
  because the write half already shipped on the Protocol in 126a.

## Findings I'm departing from (if any)

None.

## Goals

- **Be the run-start CONSUMER of 126a's durable user-prompt field.** Read the
  caller's active USER-scope config revision via 126a's existing registry read
  (`Registry.Active(ctx, id, agentID, agentcfg.ConfigScopeUser)`), extract its
  `user_prompt`, and project it into the run's prompt layers. This satisfies
  Harbor's "no primitive without a consumer" rule for the durable user write
  surface 126a pinned.
- **Compose the durable layer into the EXISTING lower-trust
  `<user_instructions>` block** as a new MIDDLE segment, in precedence order
  admin Base > admin User > USER-durable > session User — reusing the existing
  composition + escaping, no new prompt block, no new escaping path. An empty
  durable layer leaves the run byte-identical to today.
- **Extend `composeUserLayer` from two ordered segments to three**
  (admin user, durable user, session user) — the ONLY new code surface beyond
  the read.
- **Change nothing about authority.** The durable write already passed through
  126a's user-scope tier; the read-side projection adds NO new auth gate, NO
  new verb, NO new scope. It reads under the run's already-verified identity.
- **Preserve the shared run-loop seam (§17.6).** Both run-loop drivers already
  delegate prompt-layer projection to the single shared `ApplyPromptLayers`;
  because the durable layer is read from the registry both twins ALREADY pass
  (no new parameter), the new behaviour reaches both twins through the one
  shared function — the §17.6-ideal outcome. A grep-backed assertion pins both
  call sites to the shared seam.

## Non-goals

- **A new user-keyed prompt store** — deleted from scope. The durable
  `user_prompt` is persisted by 126a's user-scope config revision; a second
  store is the §13 two-writers/one-reader smell.
- **A new write verb (`agent_config.user.set_prompt`), its method constant, or
  its wire types** — deleted from scope. The one durable user write surface is
  126a's `agent_config.user.set_revision`.
- **Defining the user-scope tier classifier** (`canonicalAgentConfigUserMethods`
  / `IsAgentConfigUserMethod`) or the `auth.ScopeAgentConfigUser` scope — those
  are OWNED by 126a; this phase neither defines nor references them (it touches
  no method/scope code at all).
- **Run-start projection of the durable narrow-only tool disables**
  (`disabled_servers` / `disabled_tools`) — that is Phase 126c, the sibling
  projection-only consumer of the SAME 126a payload.
- **A Console editor page / UI for the durable user layer** — a later Console
  page phase consuming 126a's verb family.
- **Admin-set Base or User layers (Phase 92e)** — unchanged; this phase only
  threads the durable user field through the existing composition.
- **Any new Protocol wire surface** — this phase adds no method, type, or
  field, so the typed TS client, the wire manifest, and the generated Protocol
  docs are untouched.

## Protocol version

No `ProtocolVersion` change, and (unlike 126a) no Protocol wire surface is
added at all — this phase adds no method, no wire type, and no field. The
durable user write surface and its readback verbs are 126a's; this phase is a
Runtime-internal run-start projection that consumes the registry. Per
`internal/protocol/types/version.go`, `ProtocolVersion` holds at `0.1.0`; RFC
§5.3 governs ONLY the trip-wire that *bumping* the pinned constant is an RFC
change — which this phase does not approach, since it touches no wire element.

## Acceptance criteria

- [ ] `composeUserLayer`
      (`internal/runtime/agentcfg/projection/projection.go:315`) is extended
      from `(adminUser, sessionUser)` to `(adminUser, durableUser, sessionUser)`,
      joining the present segments in that order with `\n\n` into the single
      existing lower-trust `<user_instructions>` string. Every subset (incl.
      all-empty → `""`, and only-durable-set) composes correctly; the prompt
      builder's escaping is unchanged.
- [ ] `ApplyPromptLayers`
      (`internal/runtime/agentcfg/projection/projection.go:276`) reads the
      caller's active USER-scope durable revision via 126a's existing registry
      read — `reg.Active(ctx, identity.Quadruple{Identity: id.Identity},
      agentID, agentcfg.ConfigScopeUser)` — extracts `user_prompt` via the
      existing `rev.Payload.UserPrompt()` accessor, and passes it as the MIDDLE
      `composeUserLayer` segment. A registry read error is RETURNED (the run
      fails loudly per §13: no silent fall-through); a nil registry / empty
      agentID / no active user revision / a revision with no user prompt yields
      `""` (the backward-compatible "no durable user layer" path), leaving the
      run byte-identical to today.
- [ ] `ApplyPromptLayers`'s exported signature is UNCHANGED — no new store
      parameter (the durable layer is read from the `reg agentcfg.Registry`
      the function already takes). The two run-loop twins therefore need no
      edit and cannot drift on this seam.
- [ ] Precedence is verified: with all four sources set, the composed user
      block is exactly `admin User` + `\n\n` + `USER-durable` + `\n\n` +
      `session User`, in that order, below the always-spine admin Base. Any
      subset composes correctly; the always-spine admin Base is never sourced
      from a caller layer (base-unwritable-by-user stays structural — the
      durable layer carries only `user_prompt`, no base field).
- [ ] Both run-loop drivers that call `ApplyPromptLayers` —
      `cmd/harbor/cmd_dev_runloop.go:601`
      (`(*perTaskRunLoopDriver).projectAgentConfigPromptLayers`) and
      `harbortest/devstack/devstack.go:1579`
      (`(*DevStackRunLoopDriver).projectAgentConfigPromptLayers`) — route
      prompt-layer projection through the SINGLE shared `ApplyPromptLayers`
      seam (§17.6). A grep-backed smoke assertion pins BOTH call sites to
      `projection.ApplyPromptLayers`.
- [ ] **Round-trip integration AC (the §13 producer→consumer pairing):** a
      `user_prompt` written through 126a's REAL durable write path
      (`agent_config.user.set_revision` / `Service.UserSetRevision`, scope
      `ConfigScopeUser`) appears in the NEXT run's composed `<user_instructions>`
      block end-to-end, in the correct precedence position, under the SAME
      `(tenant, user)` identity — with NO admin or session layer required.
- [ ] Cross-user / cross-session isolation: user A's durable layer never
      reaches user B's run (the durable revision is keyed by A's real
      `(tenant, user)`), and is INVARIANT across A's distinct sessions (the
      whole point — it spans A's sessions for the agent and is invisible to B).
- [ ] `scripts/smoke/phase-126b.sh` asserts (static) the 3-segment
      `composeUserLayer`, the `ConfigScopeUser` durable read in
      `ApplyPromptLayers`, and BOTH run-loop twins routing through
      `projection.ApplyPromptLayers`; FAIL = 0. No new route is added, so the
      smoke is static-only (the run-start behaviour is covered by the
      integration test).

## Files added or changed

```text
internal/runtime/agentcfg/projection/projection.go         # composeUserLayer 3-segment + ApplyPromptLayers reads ConfigScopeUser durable user_prompt
internal/runtime/agentcfg/projection/projection_test.go    # 3-segment ordering + ConfigScopeUser read + fail-loud-on-read-error
test/integration/phase126b_user_prompt_layer_test.go       # NEW — write via 126a set_revision -> appears in next run's <user_instructions>; cross-user isolation; -race
scripts/smoke/phase-126b.sh                                # NEW — static projection-wiring assertions
docs/plans/phase-126b-user-scope-prompt-layer.md
docs/decisions.md                                          # D-257
docs/glossary.md                                           # "durable user-scope prompt layer"
docs/plans/README.md                                       # Phase 126b row Pending (V1.6) -> Shipped on land + detail-block stub
README.md                                                  # Status table row on land (if it surfaces a reader-facing surface)
```

No new package, no new top-level directory; AGENTS.md §3 unchanged. No
`internal/protocol/*`, `web/console/*`, or `docs/site/*` change — this phase
adds no Protocol wire surface.

## Public API surface

This is a projection-only phase: the only EXPORTED symbol it touches keeps its
signature, and the one signature change is to an UNexported helper.

```go
// internal/runtime/agentcfg/projection

// ApplyPromptLayers — UNCHANGED exported signature. It already takes the
// agentcfg.Registry; the durable USER-scope layer is read from that same
// registry via Registry.Active(..., agentcfg.ConfigScopeUser), so no new
// store/parameter is threaded and both run-loop twins are untouched.
func ApplyPromptLayers(ctx context.Context, reg agentcfg.Registry, overlayStore sessionoverlay.Store, agentID string, id identity.Quadruple, ov *planner.LLMOverrides) (*planner.LLMOverrides, error)

// composeUserLayer — UNexported, extended from two ordered segments to three.
// Joins the durable admin user layer, the durable USER-scope layer, and the
// ephemeral session user layer (in that order) into the single lower-trust
// <user_instructions> block. Any segment may be empty; all-empty yields "".
func composeUserLayer(adminUser, durableUser, sessionUser string) string

// activeDurableUserPrompt — NEW unexported helper. Resolves the caller's
// active USER-scope config revision (ConfigScopeUser) and returns its
// user_prompt. nil reg / empty agentID / no active user revision / no user
// prompt -> "" (the backward-compatible "no durable user layer" path). A
// registry read error is returned so the caller fails the run loudly.
func activeDurableUserPrompt(ctx context.Context, reg agentcfg.Registry, agentID string, id identity.Quadruple) (string, error)
```

The durable `user_prompt` field, the `agentcfg.ConfigScope` discriminator
(`ConfigScopeUser`), the `AgentConfigUserPayload`, and the
`UserPrompt() (string, bool)` payload accessor are all OWNED by Phase 126a;
this phase references them, it does not define them.

> Note: the godoc on every symbol above names the FEATURE (the durable
> user-scope prompt layer), never an internal phase number / `D-NNN` / brief
> reference (AGENTS.md §13).

## Test plan

- **Unit (`projection_test.go`):**
  - `composeUserLayer` three-segment ordering across EVERY subset of the three
    inputs (none set → `""`; only durable set; admin+durable; durable+session;
    all three → `admin\n\ndurable\n\nsession`); whitespace-only segments are
    dropped; the join separator is `\n\n`.
  - `ApplyPromptLayers` reads `ConfigScopeUser` and places the durable layer
    BETWEEN the admin user layer and the session overlay, below the always-spine
    admin Base; an empty durable layer leaves the composition byte-identical to
    the pre-phase output (a regression guard using a fake registry whose
    `ConfigScopeUser` active revision is empty).
  - **Fail-loud:** a registry whose `Active(..., ConfigScopeUser)` returns an
    error makes `ApplyPromptLayers` return that error (no silent drop of the
    durable layer) — mirrors the existing admin-layer read-error path.
- **Integration (`test/integration/phase126b_user_prompt_layer_test.go`):**
  REAL in-mem `StateStore` + REAL `agentcfg.Registry` + REAL 126a durable
  write path (`Service.UserSetRevision`, scope `ConfigScopeUser`) + REAL
  `sessionoverlay.Store` behind the REAL `ApplyPromptLayers`. The load-bearing
  assertion: a `user_prompt` written via 126a's `set_revision` for user A
  shows up in A's NEXT run's composed `<user_instructions>` — with NO admin or
  session layer set (proves the durable layer alone reaches the run), then
  AGAIN with admin User + session overlay set (proves the precedence order
  admin Base > admin User > USER-durable > session User end-to-end). Identity
  propagation asserted: the durable layer keyed to A is invisible to user B
  (B's run composes WITHOUT A's layer) and is invariant across A's distinct
  sessions. ≥1 failure mode: a forced StateStore read error fails the
  projection loudly (the run never starts with a silently dropped layer). Runs
  under `-race`.
- **Conformance:** N/A — no new method / error code / wire type is defined, so
  the `internal/protocol/singlesource` checker is unaffected.
- **Concurrency / leak:** N/A as a NEW artifact — this phase builds no new
  reusable artifact (no store, no service). The shared `ApplyPromptLayers` seam
  is already covered by the existing projection concurrent-reuse tests; the
  3-segment `composeUserLayer` is a pure function (no shared state). The
  registry's concurrent-reuse contract (incl. the scope-aware read path) is
  126a's coverage.

## Smoke script additions

`scripts/smoke/phase-126b.sh` is static-only (this phase adds no route — the
durable layer's write surface is 126a's, and the run-start behaviour is
covered by the integration test):

- Static: the 3-segment composition —
  `func composeUserLayer(adminUser, durableUser, sessionUser string)` is
  present in `internal/runtime/agentcfg/projection/projection.go`.
- Static: the durable read — `ConfigScopeUser` appears in
  `projection.go` (the `ApplyPromptLayers` durable-layer read).
- Static (the §17.6 twin-parity grep, pointed at the RUN-LOOP files): BOTH
  `cmd/harbor/cmd_dev_runloop.go` and `harbortest/devstack/devstack.go` route
  prompt-layer projection through `projection.ApplyPromptLayers`.

## Coverage target

- `internal/runtime/agentcfg/projection`: ≥ 85% (the package target stays;
  the new helper + the extended `composeUserLayer` + the read-error path are
  all covered).

No other package's coverage target moves — this phase touches one production
file plus tests.

## Dependencies

- **Phase 126a** — the durable USER-scope config tier: the
  `agentcfg.ConfigScope` discriminator (`ConfigScopeUser`), the user-keyed
  revision store, the `AgentConfigUserPayload.user_prompt` field this phase
  projects, and the `Registry.Active(..., scope)` read this phase calls. 126a
  is the producer; this phase is its run-start consumer.
- **Phase 92e** — the layered system prompt (admin operator-base + admin user
  layer) and `composeUserLayer` this phase extends.
- **Phase 92g** — the session-user safe-subset overlay (the ephemeral
  per-session user layer this phase composes the durable layer ABOVE).

## Risks / open questions

- **The durable read keys by the run's identity triple, not by `agent_id`.**
  `ApplyPromptLayers` strips the run component
  (`identity.Quadruple{Identity: id.Identity}`) and passes `agent_id` as the
  registry's per-agent key — exactly as 126a's `ConfigScopeUser` keying pins
  (`agent_id` in the session slot, real `(tenant, user)` as the isolation
  principal). The isolation tuple is NOT widened. The cross-user isolation
  test is the gate.
- **A durable user layer that silently overrode operator guidance would be a
  trust-boundary regression.** Mitigated structurally: the durable layer
  carries ONLY `user_prompt` (no base field — 126a's payload has none), and it
  composes in the lower-trust `<user_instructions>` position BELOW the
  always-spine admin Base, reusing the existing escaping — it can extend, never
  weaken or precede the operator base (the same boundary the session overlay
  and the admin User layer already establish; brief 13's trust gradient).
- **Two-writers/one-reader was the trap this re-scope closes.** An earlier
  cut added a SECOND user-keyed store + a SECOND `set_prompt` verb feeding the
  same projection — a §13 "two parallel implementations" with a guaranteed
  drift bug the instant the two writers disagreed on the durable user_prompt.
  Projection-only (one writer = 126a, one reader = this phase) is the correct
  shape and is why this phase adds no store/verb/wire surface.
- **§17.6 seam.** Because `ApplyPromptLayers`'s signature is unchanged, both
  run-loop twins reach the new behaviour through the one shared function — the
  best-case §17.6 outcome (no per-twin edit, so no drift). The twin-parity
  smoke assertion pins both call sites to the shared seam so a future refactor
  can't make one twin bypass it.
- Full §16 brief pass (brief 13 + brief 11 + RFC §6.16/§5.5 + D-235
  prompt-layer security boundary + the 126a durable-tier decision D-256) when
  dispatched.

## Glossary additions

- **durable user-scope prompt layer** — a user-instruction prompt layer
  persisted per `(tenant, user)` for an agent (one field — `user_prompt` — of
  the durable USER-scope config revision Phase 126a writes), spanning that
  user's sessions. At run start it is PROJECTED (Phase 126b) into the existing
  lower-trust `<user_instructions>` block as the MIDDLE segment, in precedence
  order admin Base > admin User > USER-durable > session User. It carries no
  base field (a user caller cannot edit the operator base) and is written ONLY
  through `agent_config.user.set_revision` (one writer) and read back ONLY by
  the run-start projection (one reader).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes (no AGENTS.md/CLAUDE.md edits expected)
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on `internal/runtime/agentcfg/projection` ≥ 85%
- [ ] If multi-isolation paths changed: cross-session/cross-user isolation
      test passes (the durable layer is invisible to other users and invariant
      across the same user's sessions)
- [ ] **No new reusable artifact is built (no store/service), so no NEW
      concurrent-reuse test is required.** The shared `ApplyPromptLayers` seam
      and the registry read are already under the existing projection +
      registry concurrent-reuse coverage; `composeUserLayer` is a pure function.
- [ ] **This phase consumes a shipped subsystem's surface (126a's durable
      user-scope revision) end-to-end: an integration test exists, wires real
      drivers, writes via 126a's `set_revision` and asserts the layer reaches
      the next run's `<user_instructions>`, asserts identity propagation,
      covers ≥1 failure mode (StateStore read error), runs under `-race`.**
      `test/integration/phase126b_user_prompt_layer_test.go`.
- [ ] No Protocol wire surface changed, so `make protocol-ts-gen-check` and
      `make protocol-docs-gen-check` are expected to be no-ops (verify clean)
- [ ] §18 skills check: grep `docs/skills/` for the `protocol` / `agent-yaml`
      surface; this phase changes no operator-followed verb/flag/config surface
      (the write verb is 126a's), so no skill update is expected — confirm
- [ ] If new vocabulary: glossary updated (`durable user-scope prompt layer`)
- [ ] If a brief finding was departed from: justified above + decisions.md
      entry filed — N/A, no departures (D-257 records the projection consumer)

---

## Implementation handoff

Turnkey artifacts for the implementing agent.

### (a) Master-plan `docs/plans/README.md` index row

Insert in phase-number order (after the 126a row), matching the column format
`# | Name | Subsystem | RFC § | Deps | Cov. | Status`:

```text
|126b| USER-scope durable prompt-layer projection (PROJECTION-ONLY consumer of 126a's durable user_prompt: reads the active ConfigScopeUser revision and threads user_prompt into `<user_instructions>` as the middle segment — admin Base > admin User > USER-durable > session User; no new store/verb) | internal/runtime/agentcfg/projection | §6.16, §5.5 | 126a, 92e, 92g | 85% | Pending (V1.6) |
```

### (b) `docs/decisions.md` entry (markdownlint-clean — blank lines around the heading and the `---`)

```markdown

---

## D-257 — The durable USER-scope prompt layer is PROJECTION-ONLY: one writer (126a set_revision), one reader (run-start projection)

**Date:** 2026-06-24

**Status:** Accepted

**Context.** Phase 126a persists a durable, versioned, user-keyed config
revision and pins `AgentConfigUserPayload.user_prompt` as one of its
band-complete fields — written through the ONE durable user-scope write verb
`agent_config.user.set_revision`. But nothing reads `user_prompt` back at run
start, so the field is inert: a user can store a standing personal instruction
and it never reaches the LLM. `ApplyPromptLayers`
(`internal/runtime/agentcfg/projection/projection.go`) composes the admin Base
as the always-spine, then joins the admin User layer and the session overlay's
user prompt into ONE lower-trust `<user_instructions>` block via
`composeUserLayer`. The durable user layer has no slot in that composition yet.

**Decision.** Add the run-start CONSUMER of 126a's durable `user_prompt`,
PROJECTION-ONLY — no new store, no new verb, no new wire surface.

1. **One writer, one reader.** The durable `user_prompt` is written ONLY by
   126a's `agent_config.user.set_revision` (`ConfigScopeUser` revision) and
   read back ONLY by this projection. An earlier cut proposed a SECOND
   user-keyed store + a SECOND `agent_config.user.set_prompt` verb feeding the
   same projection — that is a §13 "two parallel implementations of the same
   conceptual feature" (two writers, one reader) with a guaranteed drift bug
   the instant the two writers disagree on the durable user prompt. Rejected.
   The durable user prompt has exactly one home: 126a's payload field.
2. **Read via 126a's existing registry read.** `ApplyPromptLayers` reads the
   caller's active USER-scope revision with
   `reg.Active(ctx, identity.Quadruple{Identity: id.Identity}, agentID,
   agentcfg.ConfigScopeUser)` and extracts `rev.Payload.UserPrompt()`. nil reg
   / empty agentID / no active user revision / no user prompt yields `""` (the
   backward-compatible "no durable user layer" path); a registry read error is
   returned so the run fails loudly (no silent drop).
3. **Precedence admin Base > admin User > USER-durable > session User.** The
   admin Base stays the always-present spine. The other three compose, in that
   order, into the SINGLE existing lower-trust `<user_instructions>` block —
   `composeUserLayer` is extended from two ordered segments to three (admin
   user, durable user, session user); the prompt builder's escaping is
   unchanged. An empty durable layer leaves the run byte-identical to the prior
   composition. The composition order is the security boundary (D-235): a
   caller layer can extend the operator's standing instruction, never precede,
   replace, or weaken the operator base. The durable layer carries no base
   field (126a's payload has none), so base-unwritable-by-user stays
   structural.
4. **No new authority surface.** The durable write already passed through
   126a's user-scope tier (`auth.ScopeAgentConfigUser`); the read-side
   projection adds NO new auth gate, scope, verb, method constant, or wire
   type. It reads under the run's already-verified identity. The exported
   `ApplyPromptLayers` signature is UNCHANGED (the durable layer reads from the
   registry the function already takes), so both run-loop twins reach the new
   behaviour through the one shared seam and cannot drift (§17.6).
5. **Consumer of the 126a primitive (the "no primitive without a consumer"
   rule).** 126a's durable user write surface is the primitive; this projection
   is the run-start consumer that makes `user_prompt` load-bearing, with a
   round-trip integration test (write via `set_revision` → appears in the next
   run's `<user_instructions>`).

**§4.3 deviations.** None beyond the re-scope: this phase ships no store, verb,
wire type, or Console surface (all explicitly deleted from an earlier cut in
favour of consuming 126a's). No new wire element, so `ProtocolVersion` is
untouched (RFC §5.3 / CLAUDE.md §8 / `internal/protocol/types/version.go`
govern only a bump, which this phase does not approach).

**Cross-references.** D-256 (126a — the durable USER-scope tier + the user
write surface this phase consumes), D-235 (the prompt-layer composition-order
security boundary), D-025 (the concurrent-reuse contract for the shared
projection seam — no NEW artifact here), D-061 (the Console is a Protocol
client; the layer's write+readback flow over 126a's Protocol verbs). RFC
§6.16, §5.5. brief 13, brief 11. Depends on Phase 126a (the durable write
surface), Phase 92e (the admin layered prompt + `composeUserLayer`), Phase 92g
(the session overlay). Plan:
`docs/plans/phase-126b-user-scope-prompt-layer.md`.
```

### (c) `scripts/smoke/phase-126b.sh` assertions to add

```bash
#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 126b smoke — the durable USER-scope prompt-layer PROJECTION. This
# phase is projection-only: it adds no store, verb, or route, so the smoke is
# static. It asserts the 3-segment composeUserLayer, the ConfigScopeUser
# durable read in ApplyPromptLayers, and that BOTH run-loop twins route
# prompt-layer projection through the single shared ApplyPromptLayers seam
# (the §17.6 twin-parity grep, pointed at the run-loop files). The run-start
# behaviour itself is covered by test/integration/phase126b_user_prompt_layer_test.go.
#
# Conventions (AGENTS.md §4.2): use scripts/smoke/common.sh helpers; FAIL = 0.

set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. The 3-segment composition (admin user, durable user, session user).
assert_grep_present 'func composeUserLayer(adminUser, durableUser, sessionUser string)' \
    internal/runtime/agentcfg/projection/projection.go \
    'phase 126b: composeUserLayer joins three ordered segments'

# 2. ApplyPromptLayers reads the durable USER-scope revision (126a's read).
assert_grep_present 'ConfigScopeUser' \
    internal/runtime/agentcfg/projection/projection.go \
    'phase 126b: ApplyPromptLayers reads the ConfigScopeUser durable layer'

# 3. The §17.6 twin-parity grep — BOTH run-loop drivers route prompt-layer
#    projection through the single shared ApplyPromptLayers seam.
assert_grep_present 'projection.ApplyPromptLayers' \
    cmd/harbor/cmd_dev_runloop.go \
    'phase 126b: prod run-loop driver routes through the shared ApplyPromptLayers seam'
assert_grep_present 'projection.ApplyPromptLayers' \
    harbortest/devstack/devstack.go \
    'phase 126b: devstack twin routes through the shared ApplyPromptLayers seam'

smoke_summary
```

> `assert_grep_present` and `smoke_summary` already live in
> `scripts/smoke/common.sh`; this phase adds no new helper. No live block —
> the phase adds no route (the write surface is 126a's), so there is no
> endpoint to probe under the 404/405/501 → SKIP convention.

### (d) Master-plan per-phase detail-block stub

Add under the Phase 126b detail section of `docs/plans/README.md`:

```markdown
#### Phase 126b — USER-scope durable prompt-layer projection

- **Subsystem:** `internal/runtime/agentcfg/projection` (the run-start
  prompt-layer projection only — no new package).
- **RFC:** §6.16 (agent config read back at run start; `agent_id` is a key,
  not an isolation principal), §5.5 (verified identity keys the read; the
  durable layer was already gated at write time by 126a).
- **Deps:** 126a (the durable USER-scope revision + `ConfigScopeUser` read +
  the `user_prompt` field this projects), 92e (the admin layered prompt +
  `composeUserLayer`), 92g (the session overlay this composes above).
- **Decision:** D-257.
- **What it delivers:** the run-start CONSUMER of 126a's durable `user_prompt`.
  PROJECTION-ONLY — no new store, verb, method, or wire type.
  `ApplyPromptLayers` reads the caller's active `ConfigScopeUser` revision via
  126a's existing registry read and threads `user_prompt` into the existing
  lower-trust `<user_instructions>` block as the MIDDLE segment, in precedence
  order admin Base > admin User > USER-durable > session User;
  `composeUserLayer` goes from two segments to three. One writer (126a's
  `set_revision`), one reader (this projection) — closing the §13
  two-writers/one-reader trap an earlier cut would have created.
- **Risks:** the durable read must key by the run's identity triple
  (`agent_id` in the session slot, never an isolation filter); the §17.6 seam
  is the shared `ApplyPromptLayers` (no signature change, so both twins reach
  the new behaviour through the one function).
- **Status:** Pending (V1.6).
```
