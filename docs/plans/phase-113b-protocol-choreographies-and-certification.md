# Phase 113b — Protocol adoption track: the pause + versioning choreographies, build-a-client, and conformance certification

## Summary

Closes the Protocol adoption track (`docs/notes/protocol-docs-proposal.md`, PR #305) on the foundation 113a laid: choreography guides 4–5 (**the pause model** — `pause.requested` → approve / reject / OAuth-callback / plain resume, durable pauses across restarts, timeout reaps; **versioning & compatibility** — RFC §5.3 made adopter-facing, including the unknown-field / unknown-method tolerance conventions the smoke scripts already encode); the **build-a-client** guide (a ~150-line worked event-viewer client, with the hand-maintained TS wire-type module — described accurately per D-132 — and the Console as reference implementations); and the **conformance-certification** page documenting `internal/protocol/conformance` as the compatibility-claim path — how to run the suite and what passing claims. No sdk-export of the suite (proposal Q3, owner-resolved). Decision: D-210 (reserved; logged at ship).

## RFC anchor

- RFC §5 — the Harbor Protocol.
- RFC §5.1 — the decoupling rule ("the same surface powers a remote attach, a third-party dashboard, or an IDE/TUI client" — the claim this phase makes acceptable-in-practice).
- RFC §5.2 — what the Protocol exposes (the event-viewer client consumes the streaming-events row; the pause guide consumes the task-control row).
- RFC §5.3 — versioning + the deprecation window (choreography 5's source of truth).
- RFC §5.4 — wire transport (the SSE + REST shapes the worked client speaks).
- RFC §5.5 — authentication (the worked client's bootstrap leg).
- RFC §3.3 — the unified pause/resume primitive (choreography 4 is that primitive seen from the wire).
- RFC §6.3 — steering + pause/resume mechanics (the intervention surfaces choreography 4 walks).

## Briefs informing this phase

- brief 02 — planner + steering + HITL: the pause taxonomy and the "one primitive for HITL / OAuth / steering PAUSE" finding choreography 4 renders adopter-facing.
- brief 11 — Console feature surface: the intervention-queue and notification surfaces define what a client that "renders pending interventions correctly" must consume; the Console is this guide's reference implementation.
- brief 07 — code-level tool calling: "the runtime owns the protocol it speaks" — the build-a-client guide teaches clients to branch on the canonical method/event vocabulary, never on private strings.

## Brief findings incorporated

- **brief 02 §pause/HITL (the unified primitive).** HITL approval, tool-side OAuth, and operator PAUSE are one runtime primitive, not three subsystems. Choreography 4 therefore documents ONE wire choreography with three intervention surfaces (`approve`/`reject`, the OAuth callback leg at `auth.CallbackPath` = `/v1/tools/oauth/callback`, plain `resume`) — a client that handles `pause.requested` once handles all three causes.
- **brief 11 §intervention queue.** "A client that renders pending interventions correctly is a client that understood Harbor" (proposal §3.3). The guide pins the minimum client obligations: subscribe to `pause.requested` / `pause.resumed`, snapshot via `pause.list`, and route the operator's verdict through the control surface — the same consumption path the Console's own queue uses.
- **brief 07 §3 (the parsing surface).** Clients tolerate what they don't know: unknown fields are ignored, unknown methods surface as 404/405 — the same conventions the smoke scripts' SKIP discipline encodes. Choreography 5 promotes those repo-internal conventions into the adopter contract.

## Findings I'm departing from (if any)

None at planning time. One owner-resolved scope pin restated (proposal Q3): the conformance suite is documented as the certification path but is **not** exported through `sdk/` in this phase — the export decision waits for a real third-party ask, and the certification page says so explicitly rather than implying a public runner exists.

### Ship-time deviations (§4.3, recorded 2026-06-11 — D-210)

- **The OAuth callback route lockstep-greps against the source constant, not the generated reference.** The plan's smoke sketch said every route the pause guide names "appears in 113a's generated reference"; the callback (`GET /v1/tools/oauth/callback`) is a provider-redirect mount, deliberately NOT a canonical Protocol method, so it has no `methods.md` row. `scripts/smoke/phase-113b.sh` pins the guide's quoted route to the exported `auth.CallbackPath` constant in `internal/tools/auth/callback.go` instead — the honest equivalent trip-wire — and the guide states the route's non-method status.
- **Wire-capture provenance, per the plan's own fallback.** The approve / reject / `DecisionTimeout` legs (SSE frames, `pause.list` snapshots, control request/response pairs) were captured from a runtime assembled with the production drivers (`harbortest/devstack` + a `deny-all`-gated tool driven through the production dispatch path) — a live `harbor dev` capture is infeasible because the dev mock LLM never emits tool calls, so no approval gate ever fires on the preflight server. The OAuth-callback leg is transcribed from the handler + its tests (`internal/tools/auth`) and the page says so. The capture harness was a throwaway; the standing gates are the 111b/111c E2Es + this phase's lockstep greps (the plan's stated test posture).
- **The worked client compile gate is in-module.** `examples/` is part of the repo module, so the gate is a direct bounded `go build ./examples/protocol-clients/event-viewer` (plus a grep-absence assert that the client carries no `hurtener/Harbor` import — its SDK-free premise) rather than the 112b external-module ceremony, which would be dishonest for a client whose point is zero Harbor imports.
- **A §17.6-posture docs fix rode along.** `task-control.md` claimed pause-shaped controls cause `task.paused` / `task.resumed`; nothing calls `MarkPaused`/`MarkResumed` on the live pause path — a parked run's task status stays `running`. The line is corrected; the pause guide documents the real semantics (`pause.list` is the authoritative park read).
- **The 113a-pages regression line lives in `phase-113a.sh`, not here.** The smoke sketch lists "113a's pages still assert green" as a `phase-113b.sh` item; the shipped script carries no such assertion because `scripts/smoke/phase-113a.sh` runs in the same preflight fleet on every commit — the regression gate exists, delegated to the script that owns those pages (recorded at the Protocol-track §17.5 checkpoint).

## Goals

### (a) Choreography 4 — the pause model (`docs/site/protocol/pause-model.md`)

The one that makes Harbor distinctive on the wire. As a wire-level sequence narrative:

- The trigger: a run parks; `pause.requested` arrives on the event stream with its typed payload (cause: HITL approval, tool-side OAuth, A2A `AUTH_REQUIRED`, operator PAUSE — one event shape for all four, per RFC §3.3).
- The snapshot: `pause.list` (`POST /v1/pause/list`) for clients that attach after the event flowed.
- The intervention surfaces: `approve` / `reject` through the control surface for HITL; the OAuth callback leg (the user-agent completes the provider flow → `GET /v1/tools/oauth/callback` → the runtime resumes the run); plain `resume` for operator pauses. Each with the actual request/response.
- Durability: pauses survive a restart (Phase 111c checkpoint wiring) — a client must treat a reconnect-then-`pause.list` as authoritative, not its in-memory event replay alone.
- Timeout reaps: a parked run past its max-park window resumes with `DecisionTimeout` (111c's sweeper) — the wire shape a client observes when nobody intervened, and why a client should render deadlines.
- What `pause.resumed` carries (how the pause resolved) and the terminal states each path leads to.

### (b) Choreography 5 — versioning & compatibility (`docs/site/protocol/versioning-and-compatibility.md`)

RFC §5.3 made adopter-facing:

- What the pinned Protocol version promises; where it surfaces on the wire (the version handshake / `runtime.info`); what bumps it (an RFC change) and the deprecation window third-party clients get.
- What a client should pin (the version it was built against; the capability set it requires) and what it must tolerate: **unknown-field tolerance** (new response fields are additive; never fail on them) and **unknown-method handling** (404/405 means "this runtime doesn't serve that surface" — degrade the feature, don't crash; the same convention Harbor's own smoke scripts encode as the SKIP discipline, cited as the normative example).
- Capability-driven composition: read `runtime.info` capabilities at attach and shape the client accordingly (the Phase 84a gate, generalised to third parties).

### (c) The build-a-client guide (`docs/site/protocol/build-a-client.md`)

The shortest credible client, worked end-to-end: a **~150-line event-viewer** (TypeScript or Go; one language in full, the other linked as a variant) that bootstraps auth, calls `runtime.info`, subscribes to `GET /v1/events` (SSE), and renders the event stream for one session — every line shown, runnable as listed. Then the two doors up: the hand-maintained TS wire-type module (`web/console/src/lib/protocol.ts`, kept in lockstep with `CanonicalWireTypes` — described accurately per D-132, NOT as generator output) and the Console itself as the full reference implementations; and the certification page as the closer. The worked client's source ships in the repo (`examples/protocol-clients/event-viewer/`) so the guide quotes a tested artifact instead of freehand prose code — the 113a recipe-cannot-lie posture applied to a client program.

### (d) The conformance-certification page (`docs/site/protocol/conformance-certification.md`)

Documents `internal/protocol/conformance` as the certification path: what the suite covers (every method, every error code, every capability, both consumer profiles — in-process and over-the-wire), **how to run it** (clone the repo; `go test ./internal/protocol/conformance/` with the documented `Factory` seam pointed at the runtime build under test), and **what passing claims** — wire-level compatibility with the pinned Protocol version, stated precisely so the claim is neither over- nor under-sold. Explicitly states the suite is in-repo today and that an sdk-exported self-certification runner is a future decision contingent on third-party demand (Q3) — the page must not promise a public runner.

### (e) Site placement + drift discipline

The four new pages join the Protocol nav section (`docs/site/.vitepress/config.ts`) under the 113a structure: Quickstart · Reference · Choreographies (now five) · Build a client · Certification. The §18 clause 113a added already binds these guides (they join the "documented surface" list for the methods/events they demonstrate); `scripts/smoke/phase-113b.sh` extends the phase-103-style trip-wires to the new pages and adds the worked-client compile gate.

## Non-goals

- **No sdk-export of the conformance suite (proposal Q3, owner-resolved).** The certification page documents the in-repo path; exporting the runner through `sdk/` waits for a real third-party ask (that future phase would follow the 112-band facade posture).
- **No new Protocol surface.** Zero methods / codes / events / types added; the pause and versioning guides document shipped mechanics (50/51, 72e, 111b, 111c, 59, 84a).
- **No second full client implementation.** One worked event-viewer; the Console remains the canonical full client. A TUI/IDE sample gallery is post-track material.
- **No versioned doc folders (Q4)** — same posture as 113a; deferred to the first breaking Protocol change.
- **No changes to the pause/resume primitive itself.** If writing the guide surfaces a wire-shape gap (e.g. a payload field a client genuinely cannot do without), that is a Protocol phase + RFC conversation, not a quiet addition here (§17.6 names it; the plan does not pre-authorize it).

## Acceptance criteria

- [ ] `pause-model.md` walks the full choreography with actual request/response pairs for all three intervention surfaces (approve/reject, OAuth callback, plain resume), documents durable pauses (reconnect → `pause.list` is authoritative) and timeout reaps (`DecisionTimeout`), and every method/event/route it names resolves against the 113a generated reference (smoke grep).
- [ ] `versioning-and-compatibility.md` covers: the version pin + where it surfaces, the deprecation window, what to pin vs what to tolerate (unknown-field tolerance + unknown-method 404/405 handling, citing the smoke SKIP convention as the normative example), and capability-driven composition via `runtime.info`.
- [ ] `build-a-client.md` presents the worked event-viewer; the client's source lives at `examples/protocol-clients/event-viewer/` and is **compile-gated** (Go: `go build`; or TS: `tsc --noEmit` under the smoke) so the listing cannot rot; the references section describes `protocol.ts` accurately per D-132 (hand-maintained lockstep, not generated).
- [ ] `conformance-certification.md` documents the run procedure and the precise pass-claim; it explicitly scopes the suite as in-repo (no public runner promised).
- [ ] The Protocol nav section lists all 113b pages; no dead links (the docs workflow's link gate stays green).
- [ ] `scripts/smoke/phase-113b.sh` extends the site trip-wires (pages exist, nav entries present, choreography-method lockstep greps) and runs the worked-client compile gate.
- [ ] §18 sweep: any skill/recipe naming the pause wire surface or the versioning posture is reconciled in the same PR (`use-the-harbor-protocol` is the expected hit).
- [ ] D-210 (reserved; logged at ship) authored; master-plan row flipped; glossary terms added.

## Files added or changed

- `docs/site/protocol/pause-model.md`, `versioning-and-compatibility.md`, `build-a-client.md`, `conformance-certification.md` — hand-written.
- `examples/protocol-clients/event-viewer/` — the worked client (~150 lines + module/manifest file).
- `docs/site/.vitepress/config.ts` — Protocol nav entries for the four pages.
- `docs/skills/use-the-harbor-protocol/SKILL.md` — cross-links to the completed track (§18).
- `scripts/smoke/phase-113b.sh` — trip-wires + the worked-client compile gate.
- `docs/decisions.md` — D-210 (reserved; logged at ship).
- `docs/glossary.md` — new terms (see Glossary additions).

## Public API surface

- N/A — no Go API. The four pages + the worked client are adopter-facing contract documentation; the wire surface they document is owned by the Protocol phases.

## Test plan

- **Unit:** N/A for Go packages (no runtime code); the worked client carries its own minimal build check (the compile gate) rather than a test suite — it is a teaching artifact, ~150 lines including its doc comment.
- **Integration:** the worked-client compile gate (smoke) proves the listing builds against the shipped wire types; the docs workflow's dead-link build is the cross-page integration gate (the same §17.1 acceptable shape Phase 103 recorded). The pause choreography's load-bearing live verification already exists — Phase 111b/111c's E2Es exercise the exact wire sequence the guide narrates; the guide's lockstep greps tie the prose to those gated surfaces instead of duplicating a second live harness.
- **Conformance:** N/A — documents the suite; does not modify it.
- **Concurrency / leak:** N/A — no runtime artifact.

## Smoke script additions

`scripts/smoke/phase-113b.sh` (`PREFLIGHT_REQUIRES: static-only` — page/nav/lockstep assertions + a bounded local compile; no booted-server dependency; flip the header to `live-server` only if the implementor adds wire-touching assertions):

- The four pages exist; `config.ts` carries their nav entries.
- Lockstep greps: every method (`approve`, `reject`, `resume`, `pause.list`, …), event (`pause.requested`, `pause.resumed`), and route (`/v1/tools/oauth/callback`) the pause guide names appears in 113a's generated reference; the versioning guide names the pinned-version surface.
- The certification page does NOT promise an sdk runner (grep-absent on `sdk/` runner phrasing — the Q3 guard).
- The worked-client compile gate: build `examples/protocol-clients/event-viewer/` (bounded, module-cached; the phase-112b external-gate pattern at miniature scale).
- 113a's pages still assert green (no regression on the track's first half).

## Coverage target

- N/A — no Go production code (the worked client is an example artifact gated by compilation, per the CLI/tooling posture; if it lands as Go, it carries no coverage gate, same as `examples/tools/`).

## Dependencies

- 113a (the track structure, the generated reference the guides link into, the nav section, the §18 clause). Same band; 113b is the consumer of 113a's reference pages — the §13 pairing read across the band.
- 50 / 51 / 72e / 111b / 111c (the pause primitive, fail-loud serialization, `pause.list`, the OAuth completion leg, durable pauses + timeout reaps — the shipped mechanics choreography 4 documents).
- 59 / 84a (versioning + the capability gate choreography 5 documents).
- 62 (the conformance suite the certification page documents).

## Risks / open questions

- **The guide-vs-mechanism honesty risk.** Choreography 4 documents subtle semantics (durable-pause reconnect authority, reap timing). If prose and code disagree, the code wins and the guide is a bug — the lockstep greps catch vocabulary drift but not semantic drift; the §18 same-PR rule on the pause wire surface is the standing guard, and the reviewer obligation is to diff the guide against the 111c plan + `docs/notes/session-model-contract.md` siblings at ship.
- **Worked-client bit-rot across wire-type evolution.** The compile gate catches type breaks; behavioral breaks (a renamed event type string) surface only via the lockstep greps. Acceptable: the event-viewer consumes the stream generically by design, minimizing string coupling.
- **Q3 pressure.** The certification page will generate the very third-party asks that justify the sdk-export. That is the intended adoption signal — when it arrives, the export is a new phase + RFC §3.6-posture decision, not scope creep here.
- **Q4 (deferred versioned docs)** — same recorded risk as 113a: the first breaking Protocol change must add per-version doc folders; the §5.3 bump checklist carries it.

## Glossary additions

- **pause model (wire choreography)** — the adopter-facing sequence `pause.requested` → intervention (`approve`/`reject` · OAuth callback · `resume`) → `pause.resumed`, with `pause.list` as the attach-time snapshot, durable across restarts, reaped on timeout with `DecisionTimeout`. The wire view of RFC §3.3's unified primitive. Phase 113b.
- **conformance certification (Protocol)** — the documented path for claiming Protocol compatibility: run `internal/protocol/conformance`'s suite against the build under test; a pass claims wire-level compatibility with the pinned Protocol version. In-repo at V1.3.x; sdk-export is demand-contingent (proposal Q3).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes (incl. the worked-client compile gate + 113a's track assertions still green)
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target — N/A (no Go production code)
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A (docs + example client only)
- [ ] Concurrent-reuse test — N/A (no reusable runtime artifact)
- [ ] Integration test (§17): the compile gate + the docs workflow's link gate cover the seams this phase opens (docs ↔ wire vocabulary; example ↔ wire types)
- [ ] Glossary updated (the two terms above)
- [ ] D-210 (reserved) logged at ship; master-plan row flipped; §18 swept
