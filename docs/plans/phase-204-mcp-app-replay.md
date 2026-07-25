# Phase 204 — MCP App replay in session-history hydration (HA-40)

## Summary

A rendered `ui://` MCP App VANISHES when a session is reopened: the reopened
transcript degrades to the deliberately-terse model-facing text the tool
emitted for the LLM (the rich payload lives in `structuredContent`, out of
model context by design), so the turn reads as broken or empty. This phase
reduces the durable `mcp.app_available` event in `reduceHistoryTurns`, carries
the reconstructed ref onto the hydrated message, and re-mounts the renderer
that already exists — and it DEFINES THE MISS: an App whose persisted tool
context can no longer be resolved renders a stable, honest placeholder instead
of a blank bubble or a half-mounted iframe.

**Verdict: ZERO-WIRE.** Nothing needs to be newly stored: the tool context is
already persisted as a session-scoped `StateRecord` under a deterministic
content-hash key (`internal/mcpconsole/toolcontext.go`), and
`mcp.app_available` is a registered canonical bus event
(`internal/tools/drivers/mcp/events.go`) that `state.history` already replays
with its `ServerID` / `ToolCallID` / `ResourceURI` / `DisplayMode` /
`RawHTMLTrusted` keys intact (`projectStateEvent` applies no type filter and
passes the payload through). The reduction gap is purely in the Console.

**Not Console-only, though — the honest miss path exposed a production wiring
bug and this phase fixes it (§17.6).** Defining "an unresolvable context means
the record is gone" turns the correlation id into a PROMISE, and two paths were
breaking it: the runtime-attach path (`MCPConnectionAttacher`, the
`add_mcp_connection` flow) built `AttachDeps` with **no** `ToolContext`
(`internal/runtime/serve/mcp_attacher.go`), while the driver stamped the id
unconditionally (`internal/tools/drivers/mcp/mcp.go`) — so every app declared
by a tool on a runtime-added server advertised a context that had never been
written, and the new placeholder would have turned a pre-existing degradation
(a dataless app) into a total render failure. Both halves are fixed here rather
than deferred: the attacher threads the store, and the id is stamped only when
a record actually landed.

## RFC anchor

- RFC §7
- RFC §7.3
- RFC §6.4
- RFC §6.13

## Briefs informing this phase

- brief 06
- brief 11
- brief 12

## Brief findings incorporated

- brief 06 §5 ("Two-channel split"): the live view and the reopen must both
  reduce the SAME one-bus records. Phase 161/165 established that discipline
  for stats, tool badges, and reasoning steps; this phase extends it to the
  MCP App reference — the reopen reconstructs from the identical
  `mcp.app_available` event the live SSE decoder consumes, never a second
  store, never a re-fetch of a live-only projection. The cross-producer vitest
  (`the replayed ref equals the LIVE decoder + page projection`) is what keeps
  the two honest.
- brief 11 (Console feature surface): every Console-rendered datum reaches it
  through a typed Protocol method/event. The replayed App is no exception —
  the ref comes from a canonical event, the document from
  `mcp.servers.read_resource`, the data from `mcp.apps.tool_context`. No new
  method, no private hook, no Console-side shadow store of runtime entities
  (RFC §7 / D-061).
- brief 11 §PG-3 (DisplayMode honouring): the negotiated display mode is part
  of the App's identity in the transcript, so the replayed ref carries the same
  `inline` / `fullscreen` / `pip` hint, normalising an unknown value to `''`
  exactly as the live decoder does. A reopened App must not silently
  renegotiate into a different region.
- brief 12 (Console deployment + shared UI, D-091): the renderer registry lives
  in `web/console/src/lib/chat/renderers/` and imports NOTHING outside the chat
  module; the Protocol surface is the INJECTED `MCPAppHostClient`. The miss
  placeholder therefore lands INSIDE the chat module (a renderer state), not as
  a Console page concern — the packed dev UI gets the same honest behaviour for
  free.

## Findings I'm departing from (if any)

None from the briefs.

One in-tree note is **corrected**, not departed from: HA-39's open
sub-question observes that the tool-context read path is built for eviction
that the write path never performs (no TTL, no sweeper in
`internal/mcpconsole/toolcontext.go`). This phase does NOT settle the retention
policy — it settles only the CONSOLE's behaviour when the read misses, which is
required regardless of which way retention lands, because `not_found` is
already reachable today for an unknown id, a cross-identity id, and an
in-memory state driver that restarted. The retention decision stays open and
belongs with HA-39.

## Goals

- **Make `HistoryTurn` able to represent an App at all.** It gains
  `app?: MCPAppRefView` + `serverID?: string` — deliberately the SAME
  `MCPAppRefView` the live path builds (`$lib/chat/renderers/app-bridge-host`),
  NOT a second wire shape, so the hydrated message field is
  assignment-compatible with the live path's and `MessageBubble` needs no
  change.
- **Reduce `mcp.app_available`.** `reduceHistoryTurns` folds the event into the
  run's turn: `resourceUri`, the normalised `displayMode`, `rawHtmlTrusted`,
  and the `toolCallId`, plus the `serverID`. PascalCase/snake tolerant like
  every sibling fold. LAST-WINS within a run, mirroring the live reducer (which
  overwrites the bubble's `app` on each discovery). A frame missing `ServerID`
  or `ResourceURI` declares NO app — the same guard `decodeAppAvailable`
  applies, because both are load-bearing for the mount.
- **Hydrate it.** `hydratePastTurns` sets `app` + `serverID` on the reopened
  agent message, and its render gate widens to `turn.answer || turn.terminal ||
  turn.app` so an App-only turn cannot fall through and vanish.
- **Re-mount reads the PERSISTED context by its deterministic id.** No new
  storage and no caller-controlled identifier: the renderer pairs the replayed
  `serverID` with the ref's content-hash `toolCallId` and calls
  `mcp.apps.tool_context`, exactly as the live render does.
- **Define the miss behaviour (the part reviewers will scrutinise).** The
  renderer resolves the tool context BEFORE it mounts the iframe. Four
  outcomes, all explicit:
  1. **Resolved** → the iframe mounts, the bridge is built, the context is
     delivered after `ui/notifications/initialized`.
  2. **`null`** (the adapter's mapping of the Runtime's `not_found` — unknown /
     cross-identity / evicted) → **the MISS**: `loadState = 'unavailable'`
     renders a stable placeholder ("This view is no longer available" + why the
     rest of the turn is still trustworthy), NO iframe is created and NO bridge
     is ever constructed. Announced via `role="status"`, marked
     `data-testid="mcp-app-unavailable"`, styled from tokens as informational
     (muted), not an error — the turn succeeded; only its interactive view
     cannot be rebuilt.
  3. **Throws** (any non-`not_found` Protocol error, e.g. an identity-scope
     rejection) → the existing loud error state with the message. A failure is
     never laundered into the eviction copy.
  4. **No `toolCallId` on the ref** → the app mounts and performs no delivery,
     unchanged from today. Nothing was ever captured, so nothing has gone
     missing; this is deliberately NOT the miss path.

  The resolve-then-mount ordering is what makes outcome 2 observable instead of
  silent: the previous shape mounted first and let the host fetch after the
  handshake, so a miss produced a live-looking iframe whose data simply never
  arrived.
- **Make the correlation id honest, so outcome 2 means what it says (§17.6).**
  A non-empty `tool_call_id` becomes a PROMISE that a context record exists:
  (a) `MCPConnectionAttacher` threads the runtime's tool-context store into
  `AttachDeps` (production + `harbortest/devstack`), closing the runtime-attach
  gap so an app declared by a tool on a server attached via
  `add_mcp_connection` captures exactly as a boot-config one does; (b) the MCP
  driver stamps the id ONLY after the capture succeeded — no capturer wired, or
  a failed capture, yields NO id, which routes the reader to outcome 4 (mount,
  no delivery) instead of outcome 2. Without both, an honest miss path would
  report "this view is no longer available" for contexts that were never
  written — a total render failure where the old behaviour merely showed an
  empty app.
- **Keep a stale preload from tearing down a live bridge.** `preload` awaits
  twice, so an `app` prop-identity change mid-flight leaves two preloads in
  flight; `loadState` is a tracked dependency of the bridge lifecycle effect, so
  a stale write of a terminal state fires its cleanup and closes a bridge
  mid-`ui/initialize` — the D-342 revert shape reached through a data outcome
  rather than a theme change. A monotonic in-flight token makes every post-await
  write a no-op for a superseded preload. The dedup key becomes
  `resourceUri` + `toolCallId` (two calls can declare the same document with
  different contexts, and the fold is last-wins), and the `loadState` read in
  that guard is explicitly `untrack`ed rather than relying on `&&`
  short-circuit — operand order was silently load-bearing.
- **Keep the replayed App's posture identical to a live one.** Same sandbox
  tokens (never `allow-same-origin`), same CSP, same trust flag, same
  negotiated display mode, same document — pinned by a render-level equivalence
  test that mounts both messages through the real `MessageBubble`.

## Non-goals

- **Resolving heavy `artifact_ref` payloads in the data-delivery path (HA-39).**
  A tool context at/above the heavy threshold rides by reference and the
  Console's resolution depends on presigned URLs, which only the S3 artifact
  driver implements. A replayed App inherits that limitation exactly as a live
  one does — no better, no worse. Blocked on a driver-independent byte-read
  method; explicitly out of scope here (see Risks).
- **The tool-context retention/eviction policy** — HA-39's open sub-question.
  This phase defines the Console's miss behaviour, which is required either
  way; it does not decide whether records expire.
- **Progressive `tool-input-partial` streaming into a rendered App** — RESERVED
  as D-343, untouched.
- **`ui/notifications/size-changed` (HA-38), app→host `tools/call` namespace
  confinement (HA-41)** — separate asks against the same host, not bundled.
- **Any wire or Protocol change.** No new method, type, error, or event; no
  `ProtocolVersion` bump; therefore no D-223 lockstep churn and no D-209 docs
  regen. A manifest or generated-docs diff in this PR is a red flag. (Go source
  IS touched — the §17.6 capture-wiring fix above — but only behaviour behind
  existing shapes.)

## Acceptance criteria

- [x] `HistoryTurn` gains `app?: MCPAppRefView` + `serverID?: string`, reusing
  the live `MCPAppRefView` type (no second wire shape declared).
- [x] `reduceHistoryTurns` folds `mcp.app_available` into the run's turn:
  `resourceUri` / normalised `displayMode` / `rawHtmlTrusted` / `toolCallId` +
  `serverID`, PascalCase and snake_case tolerant, last-wins per run, and
  declares NO app when `ServerID` or `ResourceURI` is missing.
- [x] Cross-producer pin: the replayed ref EQUALS what the live
  `decodeAppAvailable` + `applyAppAvailable` projection builds for the same
  payload (same URI, mode, trust posture, correlation key, server).
- [x] Ordering / interleaving preserved: an App-bearing turn's tool-call rows,
  answer deltas, stats, and terminal state are unchanged by the new fold; two
  runs in one window keep independent Apps.
- [x] Page-boundary safety: a run whose discoveries straddle two loaded
  `state.history` windows resolves to exactly ONE App (the later discovery
  winning), with no duplication and no reorder of the surrounding rows.
- [x] `hydratePastTurns` sets `app` + `serverID` on the reopened agent message
  and its render gate admits an App-only turn.
- [x] Rehydration regression at the RENDER level: the real `MessageBubble`
  mounts the real `McpAppRenderer` for a replayed message, resolving the same
  document from the same server and the same persisted context by the same
  deterministic id as the live message, with byte-identical `srcdoc`, sandbox
  tokens, trust flag, and display mode.
- [x] MISS behaviour: an unresolvable context (`toolContext` → `null`) renders
  the `mcp-app-unavailable` placeholder with `role="status"`, mounts NO iframe,
  constructs NO bridge, and leaves the rest of the turn (the answer text)
  intact. A THROWING read surfaces the loud error state with its message
  instead. A ref with no `toolCallId` mounts as before with no delivery and no
  placeholder.
- [x] The correlation id is a promise, both halves: a runtime-ATTACHED
  connection captures its app tool contexts (proved against a real
  streamable-HTTP MCP fixture whose tool declares a `ui://` app on its
  definition, asserting the capture happens under the CALLER's identity and the
  discovery event advertises the id the record is keyed by), and the driver
  stamps NO id when no capturer is wired or the capture failed — while still
  publishing the discovery, so the app renders with no delivery.
- [x] Concurrency: with two preloads in flight, only the current one writes
  `loadState`; a stale one neither mounts over a newer miss nor closes the
  bridge the current one built (both interleavings pinned, both fail when the
  in-flight guard is removed).
- [x] ZERO wire change: `make protocol-ts-gen-check` and
  `make protocol-docs-gen-check` produce no diff; no new method/type/event and
  no `ProtocolVersion` bump.
- [x] `npm run check` (svelte-check `--fail-on-warnings`), `npm run lint`
  (chat-encapsulation guard, protocol lockstep, stylelint token rules, eslint),
  `npm run test`, and `npm run build` are clean; runes mode only, no raw
  color/spacing literals.
- [x] `scripts/smoke/phase-204.sh` OK ≥ 2, FAIL = 0.

## Console consistency

Per CLAUDE.md §4.5 item 12 this phase is built against
`docs/design/console/CONVENTIONS.md`, and diverges from none of it:

- **Routing / app shell** — no route and no shell change. The work lands inside
  the existing `(console)/playground/[session_id]` page and the shared chat
  module; no new page, no `/console/` prefix.
- **Shared component inventory / the four-state `<PageState>` contract** — the
  page's async contract is untouched. The App placeholder is a RENDERER state
  (`loading` / `ready` / `empty` / `error` / `unavailable`) inside
  `mcp-app.svelte`, matching that component's existing state vocabulary rather
  than introducing a competing one. No hand-rolled primitive is added; no
  component the chosen library provides is rebuilt.
- **Typed client layer** — every call goes through the injected
  `MCPAppHostClient` over the unified `HarborClient` (D-173): no hand-rolled
  `fetch`, no direct MCP transport, no Console-specific singleton reaching into
  the chat module.
- **Design tokens** — the placeholder reuses the renderer's existing
  `.mcp-app__state` block and references only `tokens.css` custom properties
  (`--color-text-muted`, `--space-*`, `--text-*`). No raw color / spacing /
  type-scale literal; stylelint gates it.
- **Error handling** — the miss is surfaced, never swallowed: a resolvable
  failure gets the loud error state with its message, an eviction gets a
  distinct honest placeholder, and neither degrades to a blank bubble
  (CLAUDE.md §13).
- **Svelte 5 runes only** — the new renderer state is `$state` / `$derived`; no
  `$:`, no `export let`, no store auto-subscription. `svelte-check
  --fail-on-warnings` gates it.

## Files added or changed

- `web/console/src/lib/sessions/history.ts` — `HistoryTurn.app` +
  `HistoryTurn.serverID`; the `mcp.app_available` branch in
  `reduceHistoryTurns`; a `readBool` reader and the `KNOWN_DISPLAY_MODES`
  normaliser.
- `web/console/src/lib/sessions/tests/history-mcp-app.spec.ts` (new) — the
  reducer fixtures: shape, live-projection equivalence, display-mode
  normalisation, snake_case tolerance, missing-field drop, interleaving,
  last-wins, per-run isolation, page boundary, leave-and-return, content-free.
- `web/console/src/routes/(console)/playground/[session_id]/turn-projection.ts`
  (new) + `.spec.ts` (new) — the two projections that build a turn's bubble,
  EXTRACTED from the page so both are importable and testable: the live
  `appViewFromDiscovery` and the replay `hydratedAgentMessage` (which owns the
  render gate and the App field mapping). Inline in the component they were the
  one part of the feature no spec could reach.
- `web/console/src/routes/(console)/playground/[session_id]/+page.svelte` —
  `applyAppAvailable` and `hydratePastTurns` now call those helpers.
- `web/console/src/routes/(console)/playground/[session_id]/mcp-app-replay.spec.ts`
  (new) — the render-level rehydration regression (replay ≡ live) + the miss
  placeholder + the no-App case.
- `web/console/src/lib/chat/renderers/mcp-app.svelte` — resolve the tool
  context BEFORE mounting; the `unavailable` state + its placeholder markup and
  token-only style; the resolved context handed to the bridge; the in-flight
  preload token; the dedup key widened to `resourceUri` + `toolCallId` and its
  `loadState` read made explicitly `untrack`ed.
- `internal/tools/drivers/mcp/mcp.go` + `content.go` + `events.go` — stamp the
  tool-call id only when the capture succeeded (`captureToolContext` now
  reports it), with the godoc on all three id-carrying shapes stating the
  promise.
- `internal/protocol/types/mcp_apps.go` — the same promise documented on the
  wire `MCPAppRef.tool_call_id` (comment only; no shape change).
- `internal/runtime/serve/mcp_attacher.go` + `serve.go`,
  `harbortest/devstack/devstack.go` — thread the tool-context capturer into the
  runtime-attach path (with an explicit nil check so a typed-nil store never
  reads as "a capturer is wired").
- `internal/tools/drivers/mcp/toolcallid_promise_test.go` (new),
  `internal/runtime/serve/mcp_attach_toolcontext_test.go` (new) — the emitting
  side of the promise and the real-fixture runtime-attach capture test.
- `web/console/src/lib/chat/renderers/app-bridge-host.ts` —
  `AppBridgeHostOptions.toolCallId` → `toolContext` (an already-resolved
  context); `#deliverToolContext` delivers instead of fetching; the
  `MCPAppHostClient.toolContext` contract documents the miss.
- `web/console/src/lib/chat/renderers/mcp-app-tool-context.spec.ts` (new) — the
  four resolution outcomes at the renderer level.
- `web/console/src/lib/chat/renderers/app-bridge-host-injection.spec.ts` —
  delivery specs re-pointed at the prefetched context; the in-flight-close race
  test re-aimed at the surviving async window (the heavy by-reference fetch
  between the two sends).
- `scripts/smoke/phase-204.sh` (new).
- `docs/plans/README.md` — Phase 204 row + detail block.
- `docs/decisions.md` — D-348.
- `docs/glossary.md` — "MCP App replay".
- `docs/notes/downstream-asks.md` — HA-40 State: Filed → Planned (phase 204).
- `docs/skills/drive-the-playground/SKILL.md` — the reopen behaviour + the miss
  placeholder (§18 same-PR surface rule; surface: `playground`).

No wire types, no generated artifacts. The Go files above change behaviour
behind EXISTING shapes only (a constructor parameter, a stamping condition,
godoc) — no method, error, event, or wire-type surface moves.

## Public API surface

- None on the wire — no new Protocol methods, types, errors, or event types.
  The replay is Console-internal over an already-flowing, already-registered
  canonical event and two already-shipped methods
  (`mcp.servers.read_resource`, `mcp.apps.tool_context`).
- Console-internal: `HistoryTurn.app` / `HistoryTurn.serverID` (reducer output,
  reusing the live `MCPAppRefView`), and the chat module's
  `AppBridgeHostOptions.toolContext` replacing `toolCallId` — one mechanism for
  delivery, not two (§13).

## Test plan

- **Unit (Console vitest):** `history-mcp-app.spec.ts` — the fold's shape;
  equivalence with the live `decodeAppAvailable` + page projection;
  display-mode normalisation (unknown → `''`); snake_case tolerance; NO app
  when `ServerID` / `ResourceURI` is missing; ordering + interleaving with tool
  rows and answer deltas; last-wins; per-run isolation across two runs in one
  window; page-boundary (no dup, no reorder); leave-and-return (App + stats +
  badges together); content-free.
  `mcp-app-tool-context.spec.ts` — the four resolution outcomes at the renderer
  level, including "no bridge is ever constructed" on the miss.
  `app-bridge-host-injection.spec.ts` — delivery from a prefetched context, no
  delivery when none was supplied, and the stale-close drop.
- **Integration (Console render-level rehydration):** `mcp-app-replay.spec.ts`
  — real `MessageBubble` + real `McpAppRenderer` + real reducer; replay ≡ live
  on document, server, persisted-context id, `srcdoc`, sandbox tokens, trust
  flag, display mode; the miss placeholder with the answer text intact; a
  no-App turn showing no slot.
- **Integration (Go, real drivers on the seam):**
  `internal/runtime/serve/mcp_attach_toolcontext_test.go` — the PRODUCTION
  attacher against a REAL streamable-HTTP MCP fixture whose tool declares a
  `ui://` app on its DEFINITION's `_meta.ui` slot (§17.8 — the canonical
  placement, not an implementer's interpretation): the runtime-added connection
  captures the invocation's input + lowered result under the CALLER's identity
  (not the attacher's default), and the discovery event advertises the id the
  record is keyed by. The failure mode: an attacher with no capturer still
  attaches and still discovers, but advertises no id.
  `internal/tools/drivers/mcp/toolcallid_promise_test.go` — the emitting side:
  no capturer and a failing capture both leave the id empty while the discovery
  still publishes; a successful capture stamps it and it resolves to the record.
- **Conformance:** N/A — no driver seam introduced; the touched driver keeps its
  existing interface.
- **Concurrency / leak:** the Console concurrent-preload guard (both
  interleavings). Go side: no new compiled artifact and no new shared state —
  the attacher's added field is set once at construction, and the driver's
  change is a local branch on an existing per-call value; the shipped D-025
  suites (`TestProvider_ToolContextCapture_ConcurrentReuse` N≥128,
  `TestMCPConnectionAttacher_ReAttach_Concurrent` N=100) cover the surfaces and
  still pass under `-race`.

## Smoke script additions

Console-dominated phase. The reduction and the placeholder are Console-side and
are pinned by the vitest suites above (the Go-side capture wiring is pinned by
its own `-race` tests); the smoke's honest job is to prove the
SERVER-SIDE inputs the replay depends on are alive — it deliberately asserts
little else rather than padding:

- live-server:
  1. `POST /v1/state/history` without a bearer → **401** (the route is mounted
     AND identity-mandatory; 404/405/501 → SKIP).
  2. `POST /v1/control/mcp.apps.tool_context` without a bearer → **401** (the
     persisted-context read the re-mount depends on is mounted and
     identity-mandatory; 404/405/501 → SKIP).
  3. Conditional: when the dev session's `state.history` window contains an
     `mcp.app_available` event, assert it carries the `ServerID` /
     `ResourceURI` / `ToolCallID` payload keys the reducer folds. SKIP when the
     dev runtime has no App-declaring MCP server attached (the CI default) —
     the fold itself is pinned by the Console vitest.
- Done-definition: `OK ≥ 2, FAIL = 0`.

## Coverage target

The binding bar is the named vitest suites (`history-mcp-app.spec.ts`,
`turn-projection.spec.ts`, `mcp-app-tool-context.spec.ts`,
`mcp-app-replay.spec.ts`, the updated `app-bridge-host-injection.spec.ts` and
`mcp-app-theme-lifecycle.spec.ts`), gated by the frontend CI job (`npm ci && npm
run check && npm run lint && npm run test && npm run build`).

Go side: the two touched packages keep their existing targets
(`internal/tools/drivers/mcp`, `internal/runtime/serve` — 80%); this phase adds
tests to both and removes none, so coverage moves up, not down. `go test -race`
on both is part of the gate.

## Dependencies

- 125 (D-254 — the `state.history` windowed read that delivers the
  `mcp.app_available` rows).
- 161 (D-293 — `HistoryTurn` / `reduceHistoryTurns` / `hydratePastTurns`, the
  rehydration foundation this extends) and 165 (D-298 — the sibling fold whose
  discipline this mirrors).
- 109b / 109d (the MCP Apps host renderer + the `mcp.app_available` discovery
  event), 109l / D-342 (the handshake-safe bridge lifecycle + Data Delivery
  this re-orders), D-225 (tool-context capture — the persisted record the
  re-mount reads), D-173 (the injected client / no direct transport).

## Risks / open questions

- **Heavy by-reference payloads (HA-39) — a SHARED, inherited limitation.** A
  tool context at/above the heavy threshold (D-026) reaches a rendered App as a
  by-reference stub, and the Console's resolution path depends on presigned
  URLs, which only the S3 artifact driver implements; on the default
  inmem/fs/SQLite stores the host delivers a faithful `[artifact … —
  unavailable on this store]` block instead of the bytes. A REPLAYED App
  inherits exactly this — no worse than a live one, and no better. Deliberately
  not addressed here: it is HA-39, blocked on a driver-independent byte-read
  method landing in a later wave.
- **The miss path's blast radius on the LIVE path — the review finding, and
  why it is now closed.** Resolving the context before mounting means a LIVE App
  whose context cannot be resolved shows the placeholder instead of an empty
  shell. That is intended for a genuinely-gone record — but it is a REGRESSION
  wherever the id was advertised for a record that never existed, and that was
  reachable: the boot-config path wires the capturer
  (`internal/runtime/assemble`) while the runtime-attach path did not, and the
  driver stamped the id unconditionally, so an operator who attached an MCP
  server from the Console (a documented v1.21 flow) would have gone from "the
  app mounts dataless" to "the app never renders". Closed at the source rather
  than by weakening the placeholder: the attacher threads the store, and the id
  is stamped only after a successful capture. What remains is outcome 4 — a ref
  with NO `toolCallId` mounts with no delivery — which now covers every
  no-capture case (unwired embedder, transient capture failure) instead of
  covering it by accident.
- **`mcp.app_available` retention.** The replay is only as good as the durable
  window: a session whose events aged past the retained head loses the App
  along with the rest of that turn, and the page already surfaces that honestly
  via the "Older messages were trimmed by retention" notice. No new failure
  mode, but worth stating — the App is not durable independently of the event
  stream.
- **Tool-context retention is genuinely undecided (HA-39's sub-question).**
  Records are written as ordinary session-scoped `StateRecord`s with no TTL and
  no sweeper, while the read path is documented for eviction. This phase makes
  the Console correct under BOTH answers; the policy decision stays with HA-39.
- **Last-wins on multiple discoveries per turn.** The live reducer overwrites
  the bubble's `app` on each discovery, so the reopen matches it. If a future
  phase renders several Apps per turn, BOTH reducers change together — the
  cross-producer test is what will catch a one-sided change.

## Glossary additions

- "MCP App replay" (`docs/glossary.md`, same PR).

## Pre-merge checklist

- [x] `make drift-audit` passes
- [ ] `make preflight` passes — deferred to CI on PR open (a sibling agent
      holds the preflight port during this round); `make vet build`,
      `make check-mirror`, and `make drift-audit` run locally and pass.
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage: the named vitest suites pass under `npm run test`; the two
      touched Go packages gain tests and lose none, and pass `go test -race`.
- [x] If multi-isolation paths changed: no identity path changed shape, and the
      capture the attacher now enables rides the CALLER's ctx identity — pinned
      by the attach test (the record is captured under `(t,u,s)`, never the
      attacher's default). The `mcp.apps.tool_context` read the re-mount
      performs is identity-scoped server-side exactly as the live read is; a
      cross-identity id is `not_found` → the placeholder.
- [x] **Reusable-artifact concurrent-reuse:** no new compiled artifact. The
      attacher's added field is set once at construction (its documented
      immutable-collaborator shape); the driver's change is a local branch on a
      per-call value. The shipped N≥128 / N=100 D-025 suites on both surfaces
      still pass under `-race`.
- [x] **Integration test:** two — the render-level rehydration regression
      (`mcp-app-replay.spec.ts`, real reducer + real components + the miss
      failure mode) and the Go attach test (`mcp_attach_toolcontext_test.go`,
      the production attacher against a real streamable-HTTP MCP fixture, with
      identity propagation and the no-capturer failure mode), both under
      `-race` on the Go side.
- [x] Zero wire diff: `make protocol-ts-gen-check` +
      `make protocol-docs-gen-check` unchanged; the Go diff moves no wire
      surface (a constructor parameter, a stamping condition, godoc).
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: none departed from; the HA-39
      correction is documented above and in D-348.
