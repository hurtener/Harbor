# Proposal: the Protocol adoption track on the docs site

> Status: proposal for owner review (2026-06-10). Nothing here is built; this
> note is the design the implementing phase plan would be authored from.

## 1. The gap, stated as the adopter experiences it

A developer who wants to build against the Harbor Protocol today — a custom
dashboard, a TUI, an IDE integration, an event pipe into their observability
stack — lands on the docs site and finds exactly one page: the
`use-the-harbor-protocol` operator skill. It is good at what it is (an
operator's task-recipe), but it is not what a client author needs. They cannot
answer, from the site: *what methods exist? what events will I receive and
what are their payloads? what does an error look like? how do I authenticate?
what happens to my client when the Protocol version bumps?* The answers exist
— in `internal/protocol/methods/methods.go` (~45 canonical method names),
`errors.go` (22 codes), `singlesource.CanonicalWireTypes` (every wire shape),
the transports, and the RFC §5 — but they require reading Go source in a repo
the adopter may never clone.

This matters more than a normal docs gap because the Protocol is Harbor's
ecosystem surface. The Console's whole architectural claim (RFC §5.1) is that
it holds no privileged access — "the same surface powers a remote attach, a
third-party dashboard, or an IDE/TUI client." That claim is only an ecosystem
invitation if a third party can actually accept it without spelunking.

## 2. Audiences (the track serves four, in this order)

1. **The evaluator** — wants the 15-minute curl tour before committing.
2. **The client builder** — dashboard/TUI/IDE; needs the complete reference +
   the streaming and control choreographies + a stability promise.
3. **The event integrator** — pipes `events.subscribe` into Datadog/their bus;
   needs the event catalog with payload shapes, filters, replay semantics.
4. **The control integrator** — drives runs from their product (start, steer,
   approve HITL); needs the task-control choreographies and the pause model.

## 3. The doc set

### 3.1 The load-bearing piece: a GENERATED contract reference

The house rule that made the TS client trustworthy applies verbatim:
hand-written API references drift; Harbor's culture is single-source +
mechanical gates. Propose `cmd/harbor-gen-protocol-docs` — a sibling of
`cmd/harbor-gen-protocol-ts`, emitting markdown into `docs/site/protocol/`
from the same canonical sources:

- **`methods.md`** — from `methods.go` + the transports' route tables: every
  canonical method name, its HTTP route + transport, control-vs-read
  classification (`IsControlMethod`), request/response wire types (linked
  into the types page), and the auth scope it demands.
- **`events.md`** — the canonical event-type catalog with payload shapes.
  *This needs one enabling change:* event types register across subsystems'
  `events.go` files; the generator gets them by importing
  `internal/drivers/prod` and reading the populated registry (plus the
  payload structs via the same reflection treatment CanonicalWireTypes
  uses). If that proves noisy, the fallback is a `singlesource`-style
  event manifest — but registry-reading is preferred (zero new bookkeeping).
- **`errors.md`** — from `errors.go`: code, HTTP mapping, when it fires,
  whether retryable.
- **`types.md`** — from `CanonicalWireTypes`: every wire struct, field-level,
  with the snake_case wire tags.

CI gate mirrors `protocol-ts-gen-check`: `make protocol-docs-gen-check`
asserts `git diff --exit-code` after regeneration — a method/error/event/type
change without regenerated docs fails the build. The §18 same-PR rule extends
to name the generated reference explicitly. **The reference cannot drift,
ever, by construction.** This is the proposal's center of gravity; everything
else is prose around it.

### 3.2 Hand-written: the quickstart

**"Speak Protocol in 15 minutes"** — pure curl against `harbor dev`:
bootstrap a dev token (`/v1/dev/bootstrap.json`), `start` a run, tail
`GET /v1/events` (SSE) and watch the planner think, `tasks.get` the result,
then one steering call (cancel or inject). Every step shows the actual
request and the actual response. Ends with three doors: the reference, the
choreographies, "build a client."

### 3.3 Hand-written: five choreography guides

The things a reference cannot teach — sequence and intent:

1. **Auth & identity** — the JWT shape (asymmetric-only), scope vocabulary
   (`admin`, `console:fleet`), the identity triple in claims, the
   session-blank connection model (D-171), the dev bootstrap vs production
   posture, what 401/403 mean per route.
2. **Streaming semantics** — SSE subscribe, identity filtering, the elevated
   fleet scope, replay/reconnect behavior, backpressure/drop policy, how
   `events.aggregate` complements the stream.
3. **Task control** — the run lifecycle as the wire sees it: start →
   event stream → terminal task state; cancel; redirect/inject; the
   nine control methods and their preconditions.
4. **The pause model** — the one that makes Harbor distinctive on the wire:
   `pause.requested` → intervention surfaces (`approve`/`reject`, the OAuth
   callback leg, plain resume), durable pauses across restarts, timeout
   reaps. A client that renders pending interventions correctly is a client
   that understood Harbor.
5. **Versioning & compatibility** — RFC §5.3 made adopter-facing: what the
   Protocol version promises, the deprecation window, what a client should
   pin and what it should tolerate (unknown-field tolerance, unknown-method
   404/405 handling — the same conventions the smoke scripts encode).

### 3.4 "Build a client" + the conformance path

A guide that walks the shortest credible client (a ~100-line TS or Go event
viewer), points at the generated TS client + the Console as the reference
implementations, and — the adoption closer — documents
`internal/protocol/conformance` as the certification path: *run the suite
against your client's runtime interactions; pass = you can claim Protocol
compatibility.* (Stretch, separate decision: exporting the conformance
runner through `sdk/` so external clients can self-certify.)

## 4. Site placement

A new top-level **Protocol** nav section: Quickstart · Reference (the four
generated pages) · the five choreographies · Build a client. The operator
skill keeps its place under Skills and cross-links. The README's Docs table
gains the Protocol row.

## 5. Drift discipline (what keeps this honest)

- The gen-check CI gate (3.1) for the reference.
- §18 gains the Protocol-docs clause (generated pages regenerate in the same
  PR; choreography guides join the "documented surface" list for the methods
  they demonstrate).
- `scripts/smoke/phase-NN.sh` for the implementing phase: site pages exist
  for every nav entry; the quickstart's curl commands are *executed* against
  the preflight dev server (the recipe-cannot-lie pattern from
  `embed-harbor-headless`).

## 6. Phasing + effort

- **Phase A (the floor, one phase plan):** the generator + gate, the four
  reference pages, the quickstart, choreographies 1–3, nav + README row.
  Roughly the shape of the 112b effort.
- **Phase B (the closer):** choreographies 4–5, build-a-client, the
  conformance-certification story (needs the sdk-export decision).

## 7. Open questions for the owner

1. Event catalog source: registry-reading at gen time (preferred) vs a
   singlesource manifest?
2. OpenAPI emission as a generator second target — worth it for tooling
   ecosystems, or scope creep at this stage?
3. Should the conformance suite become a public (sdk-exported) certification
   path in Phase B, or stay internal until a third-party client actually asks?
4. Versioned docs (per Protocol version) — defer until the first breaking
   change, or build the folder structure now?
