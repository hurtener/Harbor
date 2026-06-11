# Phase 113a — Protocol adoption track: the generated contract reference + the executed quickstart

## Summary

Opens the Protocol adoption track on the published docs site (`docs/notes/protocol-docs-proposal.md`, merged as PR #305): a **generated** four-page contract reference (`methods.md` / `events.md` / `errors.md` / `types.md`) emitted by a new `cmd/harbor-gen-protocol-docs` from the same canonical sources the Runtime compiles from, gated by `make protocol-docs-gen-check` so the reference cannot drift by construction; the hand-written **"Speak Protocol in 15 minutes"** quickstart whose curl commands are *executed* against the preflight dev server by the phase smoke (the recipe-cannot-lie pattern from `embed-harbor-headless` / D-197); choreography guides 1–3 (auth & identity, streaming semantics, task control); the new top-level **Protocol** nav section + the README Docs-table row; and the §18 amendment extending the same-PR regeneration rule to the generated reference. Decision: D-209 (reserved; logged at ship).

## RFC anchor

- RFC §5 — the Harbor Protocol (the surface this track documents).
- RFC §5.1 — the decoupling rule; the docs document ONLY what the Protocol exposes, never internals.
- RFC §5.2 — what the Protocol exposes (the reference's table of contents, mechanically derived).
- RFC §5.3 — versioning (the stability promise the quickstart's closing doors point at; the full adopter-facing guide is 113b).
- RFC §5.4 — wire transport (SSE + REST; the route tables the methods page joins against).
- RFC §5.5 — authentication (choreography guide 1's subject).
- RFC §3.6 — the public SDK facade posture: what `sdk/` is to Go embedders, the Protocol docs are to wire clients — the supported external surface, mechanically gated.

## Briefs informing this phase

- brief 07 — code-level tool calling: "the runtime owns the protocol it speaks." The methods package's own doc comment cites this framing; the generated reference is that ownership made adopter-visible.
- brief 11 — Console feature surface: the Console is the canonical Protocol client, and every feature rule there ("if the data isn't on the Protocol, the feature can't ship") doubles as the third-party-client contract this track documents.
- brief 06 — events / observability / devx: the first-minutes adoption posture; a curl-able quickstart is the Protocol's equivalent of the five-minute operator chain.

## Brief findings incorporated

- **brief 07 §1 ("the runtime owns extraction" / the elegance principle).** The Protocol method vocabulary is the Runtime's, defined once in `internal/protocol/methods/methods.go` — the generator reads that single source rather than maintaining a parallel list, exactly the discipline brief 07 motivated for the method-name namespace (the `methods.go` package doc already credits brief 07 for keeping the Protocol's vocabulary distinct from `steering.ControlType` wire strings).
- **brief 11 §"Binding conventions" item 8.** "The Console NEVER reads internal Runtime types. All data flows through canonical Protocol events / state snapshots / topology / artifacts (by reference)." The reference documents precisely that canonical surface and nothing else — a generated page can only render what the Protocol packages define, so the decoupling rule is enforced on the docs by construction.
- **brief 06 §adoption posture.** First contact decides adoption. The evaluator audience (proposal §2) gets a 15-minute curl tour with real requests and real responses; the smoke executing those curls keeps the promise true on every preflight, the same guarantee shape brief 06 motivated for the operator-skill chain.

## Findings I'm departing from (if any)

None at planning time. Two §4.3 deviations recorded at ship (D-209 calls 3–4):

- **`control.HTTPStatus` exported.** The generated `errors.md` HTTP column must read the binding the wire transport actually serves; the pre-113a `httpStatus` was unexported. Renamed-with-export (one call site updated, no behavior change, no Protocol-surface change) so the generator and the transport share one source.
- **The executed quickstart's steering step accepts both documented outcomes.** Against the preflight mock-LLM dev server a demo run reaches a terminal state in milliseconds, so the page's post-run `cancel` deterministically returns the canonical `404 not_found` envelope (steering targets live inboxes). The page teaches exactly this (acknowledgement-vs-effect; controls target live runs) and the smoke asserts the full shape of whichever outcome occurs — `200 {"accepted": true}` on a still-live run (real-provider path) or `404 {"code": "not_found"}` (mock path). The deterministic not_found leg doubles as the §17.3 failure-mode requirement.

Two clarifications that are postures, not departures:

- **The D-093 TS generator is NOT a dependency.** `cmd/harbor-gen-protocol-ts` was specified by D-093 and formally deferred (D-132 amendment; tracked in issue #179) — `web/console/src/lib/protocol.ts` is hand-maintained today. This phase mirrors D-093's *gate shape* (`git diff --exit-code` after regeneration) for a generator it actually builds; it does not build the TS generator, and nothing here blocks on issue #179. When the TS generator lands it can share this phase's `singlesource`-reflection plumbing.
- **The owner resolved the proposal's four open questions** per its recommendations (recorded here so the implementor doesn't re-litigate): Q1 registry-read event catalog (Goals below); Q2 OpenAPI deferred (Non-goals); Q3 conformance sdk-export waits for a real third-party ask (113b Non-goals); Q4 versioned docs deferred to the first breaking Protocol change (Risks).

## Goals

### (a) `cmd/harbor-gen-protocol-docs` — the generated contract reference

A sibling of the D-093-specified `cmd/harbor-gen-protocol-ts` (and of the existing `cmd/harbor-mcptest-stdio` precedent for a second `cmd/` directory), emitting four markdown pages into `docs/site/protocol/`, each carrying a generated-file header (`<!-- CODE GENERATED BY cmd/harbor-gen-protocol-docs. DO NOT EDIT. -->`) and never hand-edited:

- **`methods.md`** — from `internal/protocol/methods/methods.go` (the canonical `Method` constants + the `Is*Method` predicate family) joined against the transports' exported route-pattern constants (`control.RoutePattern`, `stream.RoutePattern`, `stream.TasksRoutePattern`, `stream.AgentsRoutePattern`, the `Memory*RoutePattern` set, etc. — every `*RoutePattern` in `internal/protocol/transports/{control,stream}`): per method, the canonical name, the HTTP route + transport, the control-vs-read classification (`methods.IsControlMethod` and the cluster predicates), the request/response wire types (linked into `types.md`), and the auth posture it demands (identity-mandatory baseline per RFC §5.5; `auth.ScopeAdmin` for the admin clusters — `IsMCPAdminMethod`, `IsToolsAdminMethod`, `auth.rotate_token`; `console:fleet` for elevated event subscriptions).
- **`events.md`** — the canonical event-type catalog, **registry-read at gen time** (proposal Q1, resolved as preferred): the generator blank-imports `internal/drivers/prod` (the §4.4 sanctioned aggregator — a `cmd/` binary entry point is exactly its intended importer), which transitively pulls every subsystem's `events.go` `init()` registrations, then reads `events.EventTypes()` for the exhaustive sorted catalog. Payload shapes get the same treatment `singlesource.CanonicalWireTypes` gets: a generator-side `eventPayloadIndex` map (`events.EventType` → payload `reflect.Type`, or an explicit no-typed-payload marker for types whose payload is a `RedactedMap` summary) rendered field-by-field via reflection, **pinned in lockstep by a test** that fails when `events.EventTypes()` gains an entry the index doesn't carry — the exact mechanism `TestSingleSource_CanonicalMethodsInLockstep` already uses for the method set. The page also documents the `SafePayload`-vs-redacted distinction (what a subscriber actually receives).
- **`errors.md`** — from `internal/protocol/errors/errors.go`: every `Code` constant (the canonical set `Codes()` returns), its HTTP mapping, when it fires, whether a client should retry.
- **`types.md`** — from `singlesource.CanonicalWireTypes` + reflection over the declaring packages (`internal/protocol/types`, plus `errors.Error`): every wire struct, field-level, with the snake_case wire tags.

The generator is deterministic (sorted iteration everywhere) so the gen-check diff is stable.

### (b) `make protocol-docs-gen-check` — the CI gate

`make protocol-docs-gen` regenerates; `make protocol-docs-gen-check` regenerates and asserts `git diff --exit-code` over `docs/site/protocol/` is clean — the gate shape D-093 specified for the TS client. Wired into CI: the gen-check runs in the docs workflow (`.github/workflows/docs.yml`, before the VitePress build, so a dirty reference fails the same job that publishes it) and the Makefile target is invocable locally. A Go-side change to a method / error code / event type / wire type without a regenerated reference fails the build. **The reference cannot drift, ever, by construction** — this is the phase's center of gravity.

### (c) The quickstart — "Speak Protocol in 15 minutes"

`docs/site/protocol/quickstart.md`, hand-written, pure curl against `harbor dev`: bootstrap a dev token (`POST /v1/dev/bootstrap.json`), `start` a run (`POST /v1/control/start`), tail `GET /v1/events` (SSE) and watch the planner think, `tasks.get` the result, then one steering call (cancel or inject). Every step shows the actual request and the actual response. Ends with three doors: the reference, the choreographies, "build a client" (113b). **The recipe cannot lie:** the smoke script extracts the quickstart's marker-tagged fenced curl blocks and executes them, in order, against the live preflight dev server (preflight already boots `harbor dev` with `HARBOR_DEV_ALLOW_MOCK=1` and exports `HARBOR_DEV_TOKEN`) — a page edit that breaks a step, or a wire change that breaks the page, fails preflight.

### (d) Choreography guides 1–3 (hand-written)

- **`auth-and-identity.md`** — the JWT shape (asymmetric-only, RFC §5.5), the scope vocabulary (`admin`, `console:fleet` — the closed D-079 set), the identity triple in claims, the **session-blank connection model (D-171)**: the connection token authenticates `(tenant, user)`, the session is chosen per-request via `X-Harbor-Session` with create-on-first-use, and the token's `session` claim is only a back-compat default; the dev bootstrap vs production posture; what 401 / 403 mean per route (`identity_required`, `scope_mismatch`, `identity_scope_required`).
- **`streaming-semantics.md`** — SSE subscribe (`GET /v1/events`), server-side identity filtering, the elevated `console:fleet` scope, replay / reconnect via the ring-buffer cursor (Phase 06 / 72), backpressure + the drop-oldest policy and the `dropped` signal, and how `events.aggregate` complements the stream.
- **`task-control.md`** — the run lifecycle as the wire sees it: `start` → event stream → terminal task state; cancel; redirect / inject; the nine control methods (`POST /v1/control/{method}`) and their preconditions; how `tasks.list` / `tasks.get` snapshot what the stream narrated.

Each choreography guide names the methods it demonstrates and links into the generated reference rather than restating shapes (one source of truth per fact).

### (e) Site placement + README

A new top-level **Protocol** nav section in `docs/site/.vitepress/config.ts`: Quickstart · Reference (the four generated pages) · the three choreographies (113b adds 4–5 + build-a-client + certification). The `use-the-harbor-protocol` operator skill keeps its place under Skills and cross-links the track. The README's Docs table gains the Protocol row pointing at the published track.

### (f) The §18 amendment + site/§18 drift discipline

`AGENTS.md` + `CLAUDE.md` §18 (mirror-gated) gains the Protocol-docs clause: the generated reference joins the same-PR regeneration rule (a Go-side method / error / event / wire-type change regenerates `docs/site/protocol/` in the same PR — mechanically backed by (b)), and the choreography guides + quickstart join the "documented surface" list for the methods they demonstrate. The Phase 103 site-manifest rule already covers the nav: new pages land with their `config.ts` entries in the same PR, and `scripts/smoke/phase-113a.sh` extends the phase-103-style trip-wires to the Protocol section.

## Non-goals

- **OpenAPI emission (proposal Q2 — owner-resolved: deferred).** A second generator target for tooling ecosystems is recorded as a stretch; nothing in the generator's design may preclude it, but no OpenAPI artifact ships in this phase.
- **Choreographies 4–5 (the pause model; versioning & compatibility), the build-a-client guide, and the conformance-certification page** — Phase 113b.
- **Conformance-suite sdk-export (proposal Q3)** — 113b documents the certification path against the in-repo suite; exporting the runner through `sdk/` waits for a real third-party ask.
- **Versioned per-Protocol-version docs (proposal Q4)** — deferred to the first breaking Protocol change (see Risks).
- **No Protocol surface changes.** This phase adds zero methods, error codes, event types, or wire types — it is a generator + docs + gate phase. (Consequence: no new live Protocol endpoint means the smoke's live obligations are the executed quickstart, not new-surface assertions.)
- **Building `cmd/harbor-gen-protocol-ts`** (D-093 / issue #179) — out of scope; this phase's reflection plumbing is reusable for it later.
- **WebSocket / stdio transport documentation** — the docs describe the shipped SSE + REST transport (RFC §5.4); alternate transports document themselves when they land.

## Acceptance criteria

- [ ] `cmd/harbor-gen-protocol-docs` emits `docs/site/protocol/{methods,events,errors,types}.md`, each carrying the generated-file header; the emitted pages are committed and byte-identical across repeated runs (deterministic ordering).
- [ ] `methods.md` covers **every** entry of `methods.Methods()` — name, route, transport, control-vs-read classification, request/response wire-type links, auth posture. A lockstep test fails if a method lacks a route or classification entry.
- [ ] `events.md` covers **every** entry of `events.EventTypes()` as populated by the `internal/drivers/prod` import (Q1 registry-read); the `eventPayloadIndex` lockstep test fails on an unindexed registered type.
- [ ] `errors.md` covers every `errors.Codes()` entry with HTTP mapping + retry guidance; `types.md` covers every `CanonicalWireTypes` entry field-level with wire tags.
- [ ] **Gen-check gate red/green proof:** `make protocol-docs-gen-check` passes on a clean tree (green), AND the phase PR demonstrates the red path — a scratch commit (or test) that adds a dummy method constant / hand-edits a generated page makes the gate fail with a diff naming the stale file. The red/green transcript is in the PR description.
- [ ] The gen-check runs in CI (docs workflow) — a Go-side wire-surface change without regenerated docs fails the build.
- [ ] **Executed quickstart:** `scripts/smoke/phase-113a.sh` extracts the quickstart's tagged curl steps and runs them in order against the preflight dev server — bootstrap → start → SSE tail observes ≥1 event for the started run → `tasks.get` → one steering call — each step asserting status + response shape (`assert_json_path`). FAIL (not SKIP) when a step's page block and the wire disagree on a build where the surface exists.
- [ ] Choreography guides 1–3 exist; guide 1 documents the D-171 session-blank model explicitly (token = who, `X-Harbor-Session` = which conversation, create-on-first-use); each guide's named methods resolve against `methods.md` (smoke grep).
- [ ] The Protocol nav section is live in `docs/site/.vitepress/config.ts` with one entry per shipped page; the README Docs table gains the Protocol row; the `use-the-harbor-protocol` skill cross-links the track (and its stale "the TS generator produces protocol.ts" claim is corrected to the D-132 hand-maintained reality in the same PR — §18).
- [ ] §18 amendment landed in `AGENTS.md` + `CLAUDE.md`, verbatim-mirrored (`make check-mirror`).
- [ ] D-209 (reserved; logged at ship) authored in `docs/decisions.md`; master-plan row flipped to Shipped; glossary terms added.

## Files added or changed

- `cmd/harbor-gen-protocol-docs/` — the generator (`main.go` + the reflection/rendering packages it needs; blank-imports `internal/drivers/prod` for the Q1 registry read).
- `docs/site/protocol/methods.md`, `events.md`, `errors.md`, `types.md` — generated, committed.
- `docs/site/protocol/index.md`, `quickstart.md`, `auth-and-identity.md`, `streaming-semantics.md`, `task-control.md` — hand-written.
- `docs/site/.vitepress/config.ts` — the Protocol nav section.
- `Makefile` — `protocol-docs-gen` + `protocol-docs-gen-check` targets.
- `.github/workflows/docs.yml` — gen-check step before the VitePress build.
- `README.md` — Docs-table Protocol row.
- `AGENTS.md` + `CLAUDE.md` — the §18 Protocol-docs clause (verbatim mirror).
- `docs/skills/use-the-harbor-protocol/SKILL.md` — cross-link + the D-093/D-132 claim correction (§18 same-PR).
- `scripts/smoke/phase-113a.sh` — site trip-wires + the executed quickstart.
- `docs/decisions.md` — D-209 (reserved; logged at ship).
- `docs/glossary.md` — new terms (see Glossary additions).

## Public API surface

- The four generated reference pages are the Protocol's adopter-facing contract surface (prose form of `methods` / `errors` / `types` / the event registry) — versioned with the repo, drift-gated by (b).
- `cmd/harbor-gen-protocol-docs` itself exposes no Go API; its lockstep maps (`eventPayloadIndex`, the route-pattern join table) are internal to the command, pinned by tests.

## Test plan

- **Unit:** generator rendering tests (golden-file per page section); determinism test (two runs, byte-identical); the lockstep tests — every `methods.Methods()` entry has a route + classification, every `events.EventTypes()` entry has an `eventPayloadIndex` entry, every `CanonicalWireTypes` entry renders.
- **Integration:** the gen-check gate self-test — a fixture tree with a stale generated page makes the check fail loudly (the §17.1 seam this phase opens is generator ↔ canonical packages; the lockstep tests + gate are its round-trip). The executed-quickstart smoke is the live cross-subsystem integration: real dev server, real auth bootstrap, real control + stream transports, identity carried on every step; the steering step (cancel) doubles as the failure-mode leg (a cancelled run's terminal state observed on the wire).
- **Conformance:** N/A — no driver seam; the Protocol conformance suite (Phase 62) is untouched.
- **Concurrency / leak:** N/A — the generator is a run-to-completion command, not a reusable runtime artifact (no D-025 obligations beyond its pure-function rendering helpers).

## Smoke script additions

`scripts/smoke/phase-113a.sh` (`PREFLIGHT_REQUIRES: live-server` — the quickstart legs hit the booted dev server; the static assertions ride along):

- Static, phase-103-style: the four generated pages exist AND carry the generated header; the five hand-written pages exist; `config.ts` has the Protocol nav section with an entry per page; `Makefile` has `protocol-docs-gen-check:`; the docs workflow invokes it; CLAUDE.md §18 names the generated reference; README Docs table has the Protocol row.
- Lockstep greps: each choreography guide's demonstrated method names appear in the generated `methods.md`.
- **Live — the executed quickstart:** extract the marker-tagged curl blocks from `quickstart.md` and execute them in order against `HARBOR_BASE_URL` with `HARBOR_DEV_TOKEN`; assert each step's status + JSON shape. 404/405/501 → SKIP only on pre-113a builds (the standing convention); once shipped, OK ≥ the quickstart's step count.

## Coverage target

- `cmd/harbor-gen-protocol-docs`: 70% (CLI/tooling default; the lockstep + golden tests carry the real guarantee).
- No other Go package's coverage moves — touched non-generator surfaces are docs and build wiring.

## Dependencies

- 103 (the docs site + the §18 site-manifest rule + the phase-103 trip-wire pattern this smoke extends).
- 58 / 59 / 60 / 61 / 62 (the Protocol single-source packages, versioning, transports, auth, conformance — all Shipped; the surfaces the generator reads and the quickstart drives).
- 110c (the `internal/drivers/prod` aggregator the Q1 registry-read imports).
- D-171 (the session model guide 1 documents), D-093/D-132 (the gate-shape precedent + the deferred TS generator this phase deliberately does not block on).

## Risks / open questions

- **Q4 (owner-resolved: deferred) — versioned docs.** The track documents the current pinned Protocol version only. The first breaking Protocol change (an RFC event per §5.3) must introduce per-version doc folders; until then a single tree is correct and cheaper. Recorded here so the §5.3 bump checklist picks it up.
- **Registry-read import weight.** Blank-importing `internal/drivers/prod` pulls the full driver tree into the generator binary. Acceptable (it's a repo-internal tool, not a shipped artifact); if init-time side effects ever make gen runs flaky, the proposal's recorded fallback is a `singlesource`-style event manifest — a documented §4.3 deviation, not a silent swap.
- **Quickstart flake surface.** SSE tailing in a smoke needs a bounded read (`curl --max-time` + first-matching-event grep), not an unbounded stream. The smoke must stay within preflight's per-script budget; the step extraction keeps page and smoke from drifting but makes the page's block tagging load-bearing — the smoke fails loudly when a tagged block count changes unexpectedly.
- **Generated-page size.** `types.md` over the full `CanonicalWireTypes` map is large; VitePress handles it, but local-search quality on one giant page is the same boundary Phase 103 flagged — split-per-cluster is the documented relief valve if search degrades.
- **The skill overlap.** `use-the-harbor-protocol` and the new track cover adjacent ground; the skill stays the operator task-recipe, the track is the adopter reference (the proposal's §1 framing). The skill correction in this PR removes its one factually-stale claim; further convergence is editorial, not structural.

## Glossary additions

- **Protocol adoption track** — the docs-site section (quickstart · generated reference · choreographies · build-a-client · certification) that makes the Harbor Protocol consumable by third-party clients without reading Go source. Phases 113a/113b.
- **generated contract reference** — the four `docs/site/protocol/` pages emitted by `cmd/harbor-gen-protocol-docs` from the canonical Protocol sources, drift-gated by `make protocol-docs-gen-check`. Never hand-edited.
- **executed quickstart** — a docs page whose command blocks are extracted and run against the live preflight dev server by the phase smoke, so the page cannot drift from the wire (the recipe-cannot-lie pattern applied to curl).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes (incl. the executed quickstart against the booted dev server)
- [ ] `make check-mirror` passes (the §18 amendment is mirrored)
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A (no isolation code path changes; the quickstart exercises the existing identity pipeline read-only)
- [ ] Concurrent-reuse test — N/A (run-to-completion generator command; no reusable runtime artifact)
- [ ] **Integration test (§17):** the gen-check self-test + the executed quickstart wire real surfaces end-to-end with identity and ≥1 failure-mode leg
- [ ] Glossary updated (the three terms above)
- [ ] D-209 (reserved) logged at ship; master-plan row flipped; §18 swept (the `use-the-harbor-protocol` correction lands same-PR)
