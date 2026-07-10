# Phase 161 — Session rehydration carries per-turn metadata

## Summary

Reopening a Playground session hydrates message CONTENT correctly but loses
everything else: the header stats show "no turns yet" (tokens/cost/latency),
the TOOL CALLS "INVOKED" badges vanish, and the model chip resets — the
operator-confirmed live-test regression. Root cause, verified at the wire and
in code: the read path does NOT strip anything (read-back ≡ published payloads
— empirically confirmed against a live dev boot), but two metadata producers
never publish onto the bus in the first place (`llm.cost.recorded` is emitted
only inside the bifrost driver; `tool.invoked/completed/failed` only by the
in-process transport driver, with an attribution-dead empty envelope run id),
and the Console's history reducer discards even the metadata that IS present
(`HistoryTurn` carries no stats fields; the reducer's own doc comment claims
`planner.*`/`tool.*` reconstruction the code never does). This phase closes
the producer gaps at ONE driver-neutral seam each (the LLM-edge wrapper chain;
the transport-agnostic dispatch seam), keeps every payload content-free (tool
NAME + status + duration only — never args/results, §7 rule 7 + D-026), and
ships the §13/D-062 consumer in the same phase: `reduceHistoryTurns` folds the
now-present keys and the Playground rehydration renders header stats, badges,
and model chip IDENTICAL to the live view. Zero wire changes: no new methods,
no new wire types, no new canonical event types.

## RFC anchor

- RFC §6.13
- RFC §5.2
- RFC §6.4
- RFC §6.5
- RFC §7

## Briefs informing this phase

- brief 06
- brief 07
- brief 11

## Brief findings incorporated

- brief 06 §5 ("Two-channel split"): "When the observability record and the
  stream chunk are separate records flowing through different paths … every
  dashboard, replay tool, and Console feature has to fuse them. Lesson: unify
  on one bus from t=0." This bug is that lesson read forward: the live view
  renders from bus events, and the reopen must reduce THE SAME bus events —
  the regression exists because two metadata producers (`llm.cost.recorded`
  in the bifrost driver only; `tool.*` in the inproc driver only) never put
  their records on the one bus for every driver/transport, so the replay path
  has nothing to fuse. The fix is one-bus discipline, not a second channel.
- brief 07 §8 (the Dispatcher owns dispatch): the tool dispatcher is "inside
  the runtime, not the LLM client" — one design unit owning validation,
  identity, deadline, and cancellation for every transport. The tool
  lifecycle emit belongs at that ONE seam (where the run quadruple is in
  hand), not per-transport-driver — the per-driver emit is why MCP/HTTP/A2A
  tools produce no lifecycle events today.
- brief 11 LR-6 (per-task detail pane): historical per-task detail — "tool
  name, source, … identity at invocation time" — is sourced "via Phase 57
  durable event log + Phase 60 protocol transport" with input/output
  post-redaction. The durable event log is the DESIGNED source for exactly
  the per-turn metadata this phase makes reconstructable on reopen; the
  redaction boundary (names + metadata yes, args/results never) is already
  drawn there.

## Findings I'm departing from (if any)

None.

## Goals

- The durable event stream's read-back carries enough CONTENT-FREE metadata
  to reconstruct on reopen: per-turn usage (tokens in/out), cost, latency,
  model id, and tool-call summaries (tool NAME + status + duration ONLY —
  never args/results; §7 rule 7 + D-026 stay inviolate; the audit-redactor
  publish path stays mandatory).
- **Gap analysis, verified (2026-07-10, live dev boot + code trace).** The
  read path strips nothing: `state_history_handler.go:297`
  (`payloadWireValue`) passes payloads through; the durable driver persists
  the full post-redaction payload map (`drivers/durable/record.go:40`/`:148`)
  and the inmem ring returns the stored payloads; a probe of a completed
  mock-run session's `state.history` page returned `llm.completion.chunk`
  with `{Delta, Kind, TaskID, RunID, …}` and `planner.decision` with
  `{DecisionKind, Tool, ReasoningTrace, …}` intact. The gaps are all
  PRODUCER-side — class (b) of the investigation frame ("events never
  published"), not (a) read-path stripping or (c) SSE-only enrichment:
  - **(b1)** `llm.cost.recorded` (payload: `Model`, `Cost`, `Usage`,
    `ContextWindowTokens` — `internal/llm/events.go:153`) is emitted ONLY
    inside the bifrost driver (`internal/llm/drivers/bifrost/bifrost.go:180`,
    `:283` → `cost.go:32`). Any other driver — the dev-posture mock, future
    providers — emits nothing, and there is no driver-neutral emit.
  - **(b2)** `tool.invoked` / `tool.completed` / `tool.failed` (payloads:
    `ToolName`, `Transport`, `Attempts`, `DurationMS`, `ErrorClass` —
    `internal/tools/events.go:52-90`) are emitted ONLY by the in-process
    transport driver (`internal/tools/drivers/inproc/inproc.go:159`/`:165`).
    MCP / HTTP / A2A tools — the live-test agent's tools — emit NO lifecycle
    events at all (verified: no other emit site exists).
  - **(b3)** Even the inproc emits are attribution-dead: they stamp
    `identity.Quadruple{Identity: id}` from the ctx TRIPLE
    (`inproc.go:185`/`:212`) — envelope `RunID` empty — and the payloads
    carry no `TaskID`, so both the live decoder
    (`wire-events.ts:118-121` `taskIDOf` → `''` → `decodeToolLifecycle`
    returns null at `:222`) and any replay reducer cannot pin the event to a
    turn. The live TOOL CALLS badges actually come from `planner.decision`
    (playground `+page.svelte` listener at `:780-793`), whose payload DOES
    carry the full quadruple + `Tool` (`react.go:757-769`).
- **Fix (runtime): one driver-neutral seam per producer.**
  - **R1 — LLM usage/cost/model:** promote the `llm.cost.recorded` emit from
    the bifrost driver to the LLM-edge wrapper chain that `llm.Open`
    composes (driver-neutral: `CompleteResponse` carries `Cost` + `Usage`,
    the request carries `Model`, the model profile carries
    `ContextWindowTokens`), and DELETE the bifrost-internal emit in the same
    change — one emit site, never two (brief 03's toggle-smell one layer
    over; no double-emission). Payload type and keys UNCHANGED. Governance is
    unaffected: cost accounting is in-band synchronous (the emit is
    observability-only — `cost.go:12-20`'s recorded posture).
  - **R2 — tool lifecycle:** move the tool lifecycle emits to the ONE
    transport-agnostic dispatch seam (`internal/runtime/dispatch/dispatch.go`
    `:238` band, where `desc.Invoke(ctx, d.Args)` dispatches every
    transport and `rc.Quadruple` is in hand), stamping the FULL run
    quadruple on the event envelope; DELETE the inproc driver's per-driver
    emits in the same change (superseded — no double emission). Payload
    shapes unchanged (name/transport/status/attempts/duration — content-free
    by construction). This gives MCP/HTTP/A2A tools lifecycle events for the
    first time AND makes every tool event turn-attributable, fixing the
    latent LIVE bug (b3) in the same PR (§17.6 fix-both-sides: the live
    status-resolution path `applyToolLifecycle` starts receiving decodable
    frames).
  - **R3 — read path: untouched.** `state.history`'s identity scoping
    (`MatchesScoped`, session-named, admin never widens) and the
    pass-through projection stay byte-identical; after R1/R2, read-back ≡
    stream holds for the named keys because both sides ARE the same bus
    events.
- **Wire/Protocol: zero changes.** No new method, no new wire types, no new
  canonical event types (all three event types are long-registered), no
  `ProtocolVersion` bump — therefore NO D-223 lockstep churn and NO D-209
  docs regen (stated explicitly; a manifest diff in the implementation PR is
  a red flag).
- **Fix (Console — the §13/D-062 consumer, same phase):**
  - `HistoryTurn` (`web/console/src/lib/sessions/history.ts:105-116`) gains
    content-free stats fields: `tokens`, `promptTokens`, `outputTokens`,
    `costUSD`, `model`, `durationMs`, and
    `toolCalls: {tool, status, summary}[]`.
  - `reduceHistoryTurns` (`history.ts:144-179`) folds the now-present keys:
    `llm.cost.recorded` → usage/cost/model sums (the same PascalCase keys
    the live decoder reads — `Usage.TotalTokens` / `Usage.PromptTokens` /
    `Usage.CompletionTokens` / `Cost.TotalCost` / `Model`,
    `wire-events.ts:145-155`); `planner.decision` `CallTool` → a tool row
    (`invoked`); `tool.completed` / `tool.failed` → resolve the row's
    status + duration/error-class summary (mirroring `applyToolLifecycle`,
    `+page.svelte:592-621`); `task.started` → `task.completed` timestamps →
    `durationMs` (with the `tasks.list` `duration_ms` catalog value as the
    primary source, as today). The reducer's doc comment (`history.ts:10-16`)
    finally becomes TRUE — it currently claims `planner.*`/`tool.*`
    reconstruction the code never performs; the claim and the code converge
    in this phase.
  - `hydratePastTurns` (`+page.svelte:908-983`) populates what it silently
    drops today: per-message `meta` (elapsed/tokens/cost) + `toolCalls` on
    hydrated agent bubbles, and the live accumulators the header/chip read —
    `turnCost`, `turnTools`, `modelName`, `tokenCount`, `promptTokens`,
    `outputTokens`, `costUSD`, `turnLatencies` (today only `activeWorkMs` is
    folded, at `:959`).
  - **Acceptance centerpiece:** leave-and-return renders header stats
    (tokens/cost/latency), per-message stats badges, TOOL CALLS badges, and
    the model chip IDENTICAL to the live view — the exact operator
    complaint.

## Non-goals

- No session-pinned-JWT Tasks/Events enumeration posture change — a separate
  follow-up decision.
- No Overview page "Recent Activity" read-back — out of scope.
- No phantom `top_p` fix — that is the wave checkpoint's MUST-FIX, already
  assigned elsewhere.
- No durable-driver-only features: everything here works on the inmem
  events driver within process lifetime (the live test ran inmem and
  `state.history` returned pages fine — verified again during this plan's
  probe); durable adds cross-restart persistence, nothing functional.
- No pre-reduced turns on the wire — D-254's "the surface returns flat
  events, reduction stays client-side" posture is preserved; this phase
  enriches what the flat events carry, not the shape of the surface.
- No new event types, no event-payload content beyond the named content-free
  keys — in particular NO tool args/results anywhere on any read-back path
  (§7 rule 7).
- No deep-history completeness guarantee: the hydration window stays bounded
  (8 pages × 50 events); metadata hydrates exactly as deep as content does
  (the window-flooding trade-off is named under Risks).

## Acceptance criteria

- [ ] **R1:** `llm.cost.recorded` is emitted at the driver-neutral LLM-edge
  wrapper seam for EVERY driver's successful `Complete` (bifrost AND the
  dev-posture mock), carrying the existing payload keys (`Model`, `Cost`,
  `Usage`, `ContextWindowTokens`) and the run quadruple on the envelope; the
  bifrost-internal emit is deleted in the same change; a test pins exactly
  ONE cost event per completed LLM call (no double emission).
- [ ] **R2:** tool lifecycle events (`tool.invoked` / `tool.completed` /
  `tool.failed` / the existing failure variants) are emitted at the
  transport-agnostic dispatch seam with the FULL run quadruple on the
  envelope, for in-process AND MCP-transport tools (the MCP leg proven
  against the real stdio fixture, §17.8); the inproc driver's per-driver
  emits are deleted in the same change; payloads carry tool NAME +
  transport + status + attempts + duration ONLY.
- [ ] **Redaction (§7):** a test drives a tool-calling run whose args and
  results contain a sentinel string, fetches the full `state.history`
  read-back, and asserts the sentinel appears NOWHERE in the page (raw
  args/results never reach any event payload); the audit-redactor publish
  path is unchanged.
- [ ] **Stream ≡ read-back:** for one completed tool-calling run, the set of
  {event type, named metadata keys} observed on a live `events.subscribe`
  stream equals the set observed in the `state.history` read-back window
  (the named keys: `Usage.*`, `Cost.TotalCost`, `Model`,
  `ContextWindowTokens`, `ToolName`, `DurationMS`, `DecisionKind`, `Tool`,
  envelope `run`).
- [ ] **Identity scoping unchanged (§6):** the existing by-id
  `MatchesScoped` posture holds — a cross-user/cross-tenant `state.history`
  read still refuses; an admin read still scopes to the named session; the
  read-path tests from the state-history phase stay green untouched.
- [ ] **Console reducer:** `reduceHistoryTurns` folds usage/cost/model/tool
  rows/duration into the widened `HistoryTurn`; vitest covers cost-fold,
  planner-decision + lifecycle status resolution (invoked → succeeded /
  failed with summary), duration fallback, and PascalCase/snake_case
  tolerance; the reducer doc comment matches the code.
- [ ] **Console rehydration regression test:** a reopen against a recorded
  event window renders header stats + per-message badges + TOOL CALLS +
  model chip from hydrated turns; the leave-and-return values equal the
  live-view values for the same run (the operator's regression, pinned).
- [ ] **Latent live bug fixed (§17.6):** with R2's envelope run id, the live
  `decodeToolLifecycle` path attributes tool status frames (previously
  dead — `taskIDOf` returned `''` for every tool.* frame); a wire-events
  vitest pins it.
- [ ] Zero wire changes verified: `make protocol-ts-gen-check` and
  `make protocol-docs-gen-check` pass with NO diff; no new method, type,
  error, or canonical event type.
- [ ] `scripts/smoke/phase-161.sh` OK ≥ 3, FAIL = 0 (see Smoke).
- [ ] `-race` on all touched Go packages; coverage ≥ 85% on touched Go
  packages.

## Files added or changed

- `internal/llm/` (the wrapper chain `llm.Open` composes — the exact file
  per the shipped chain layout, e.g. the safety/observability wrapper) —
  the driver-neutral `llm.cost.recorded` emit (R1); bus + profile deps
  threaded by the assembly.
- `internal/llm/drivers/bifrost/bifrost.go` (`:180`, `:283`) +
  `internal/llm/drivers/bifrost/cost.go` — the driver-internal emit deleted
  (folded into R1).
- `internal/runtime/dispatch/dispatch.go` (the `desc.Invoke` band, `:238`) —
  the transport-agnostic tool lifecycle emit with the run quadruple (R2).
- `internal/tools/drivers/inproc/inproc.go` (`:159`/`:165`,
  `publishToolInvoked`/`publishToolOutcome`) — per-driver emits deleted
  (superseded by R2).
- `internal/tools/events.go` — payload shapes unchanged; godoc updated to
  name the dispatch-seam emitter (no phase jargon).
- `internal/runtime/dispatch/dispatch_test.go` + `internal/llm/*_test.go` —
  the one-emit-per-call pins, the redaction sentinel test, the
  stream-vs-readback equivalence test (in-package or
  `test/integration/phase161_rehydration_test.go` — real drivers, identity
  propagation, ≥1 failure mode, `-race`, per §17.3).
- `test/integration/phase161_rehydration_test.go` (new) — completed
  tool-calling run (scripted-LLM pattern + the real stdio MCP fixture for
  the MCP leg) → `state.history` read-back asserts the named keys + the
  sentinel-redaction negative + cross-identity refusal.
- `web/console/src/lib/sessions/history.ts` — `HistoryTurn` widened;
  `reduceHistoryTurns` folds the metadata; doc comment made true.
- `web/console/src/lib/sessions/history.spec.ts` (or the existing vitest
  home) — reducer folding + rehydration regression fixtures.
- `web/console/src/routes/(console)/playground/[session_id]/+page.svelte`
  (`hydratePastTurns`, `:908-983`) — populates meta/toolCalls/KPI
  accumulators/model chip from hydrated turns.
- `web/console/src/routes/(console)/playground/[session_id]/wire-events.ts`
  — no decoder shape change expected (frames gain a populated `run`); the
  vitest pinning the now-live tool attribution lands beside it.
- `scripts/smoke/phase-161.sh` (new) — see Smoke.
- `docs/plans/README.md` — Phase 161 row + detail block.
- `docs/decisions.md` — D-293.
- `docs/glossary.md` — "content-free turn metadata".
- `docs/plans/wave-v113-coordination.md` — Stage 3 (this phase) + the
  checkpoint audit moved after it.

## Public API surface

- No new exported Go API is required by design; if the R1 wrapper needs a
  constructor option for the bus/profile deps, it is an internal assembly
  concern (`assemble.Assemble` threads it), not an `sdk/` change.
- Wire: nothing — no new methods, types, errors, or event types. The
  enrichment is the CONTENT of already-flowing, already-registered events.
- Console: `HistoryTurn` gains the stats fields (a Console-internal type,
  not a wire type).

## Test plan

- **Unit:** R1 — one `llm.cost.recorded` per completed `Complete` across two
  drivers (mock + bifrost-shaped fake at the chain seam), keys pinned, no
  double emission after the bifrost deletion; R2 — dispatch-seam emits carry
  the full quadruple + name/status/duration for a table of outcomes
  (success / failure / invalid-args / policy-exhausted), inproc emits gone;
  Console vitest — reducer folding (cost sums, tool-row lifecycle
  resolution, duration fallback, casing tolerance), `wire-events`
  attribution now decoding tool frames, rehydration regression fixture.
- **Integration (`test/integration/phase161_rehydration_test.go`):** real
  drivers (inmem events/state, patterns redactor, scripted-LLM per the 83l
  pattern; the MCP leg against the real stdio fixture per §17.8) — drive a
  completed tool-calling run, then: (1) `state.history` read-back contains
  the named content-free keys for cost + tool + decision events; (2) the
  sentinel-redaction negative (args/results never in the page); (3)
  stream-vs-readback key equivalence; (4) cross-user/cross-tenant read
  refusal unchanged (≥1 failure mode, §17.3); `-race`.
- **Conformance:** N/A — no driver-seam interface change; the events
  conformance suite stays green untouched (the emit sites move; the bus
  contract does not).
- **Concurrency / leak:** the emit sites live on per-run paths (no new
  shared mutable state); the existing dispatch + LLM-chain D-025 stress
  tests are extended to assert per-run event attribution does not bleed
  across N≥100 concurrent runs against one shared executor/client under
  `-race` (event.run always equals the invoking run's id).

## Smoke script additions

- live-server, no real LLM needed (justified: under the preflight dev
  posture the mock driver reports synthetic usage —
  `internal/llm/mock/mock.go:148` — and R1's driver-neutral emit makes
  `llm.cost.recorded` fire for the mock path too, which is itself part of
  the phase's point):
  - drive a scripted run via `control.start` against the preflight dev
    server; poll `tasks.get` to terminal;
  - fetch `state.history` for the session; assert the page contains an
    `llm.cost.recorded` event whose payload carries `Usage` + `Model` keys;
  - assert a `planner.decision` event with a `DecisionKind` key and a
    populated envelope `run`;
  - negative: assert no `Args`/`Result`/args-shaped keys appear in any
    payload in the page.
- The tool-calling leg (MCP fixture) stays in the integration test — a
  smoke cannot orchestrate the stdio fixture cheaply; the mock run above
  exercises the read-back mechanics end-to-end.
- Done-definition: `OK ≥ 3, FAIL = 0` once the phase ships; 404/405/501 →
  SKIP until then.

## Coverage target

- `internal/runtime/dispatch`: 85%
- `internal/llm`: 85%
- `internal/tools/drivers/inproc`: existing package target maintained (code
  removed, not added)
- Console: vitest suites listed above (the frontend job has no Go-style
  coverage gate; the named specs are binding).

## Dependencies

- 125 (the `state.history` windowed read this reconstructs from — D-254),
  157 (session titles — the reopened-session surface this completes; the
  Playground reopen flow this phase hardens is the same one 157's switcher
  drives), 118 (D-223 lockstep — must stay green; this phase proves a
  zero-diff), 124 (the gap-free durable substrate, via 125).

## Risks / open questions

- **Window flooding bounds metadata depth.** One turn emits O(100s) of
  `llm.completion.chunk` events but only one `llm.cost.recorded` per LLM
  call; the hydration window (8 × 50 events) therefore hydrates metadata
  exactly as deep as it hydrates content. A long session's oldest turns lose
  stats AND text together (existing, honest `truncated` behavior) — this
  phase does not change retention or window sizes. Named so a reviewer does
  not mistake bounded-window loss for the regression this phase fixes.
- **Moving emit sites has subscribers.** `tool.*` events feed the telemetry
  trace bridge (`internal/telemetry/tracebridge.go:79`) and the Console live
  listeners; `llm.cost.recorded` feeds the Playground KPI strip. The moves
  keep event types + payload shapes byte-compatible, so subscribers are
  unaffected — the equivalence test is the guard. Governance is explicitly
  NOT on this path (in-band accounting; `cost.go:12-20`).
- **Double-emission during the cutover.** Both R1 and R2 DELETE the old emit
  site in the same change; the one-emit-per-call unit pins make a
  reintroduction loud.
- **Attempts fidelity at the dispatch seam.** The policy shell (retries)
  runs INSIDE `desc.Invoke`, so the seam-level emit sees the outcome, not
  per-attempt internals; `Attempts` is derivable from the policy result
  where exposed, else reported as the shell's terminal attempt count — the
  implementor documents the choice in the payload godoc (a §4.3-recordable
  refinement, not a design change).

## Glossary additions

- "content-free turn metadata" (docs/glossary.md, same PR).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
      (the read-path scoping is untouched and re-pinned; the new emits carry
      per-run identity — the D-025 stress asserts no cross-run bleed).
- [ ] **Reusable-artifact concurrent-reuse:** the shared executor + LLM
      client chain gain the per-run attribution assertions under the
      existing N≥100 `-race` stress (no new compiled artifact is introduced;
      the existing ones' tests are extended). See §5 + §11 + D-025.
- [ ] **Integration test wires real drivers end-to-end, asserts identity
      propagation, covers ≥1 failure mode, runs under `-race`** (§17.3; the
      MCP leg per §17.8 against the real stdio fixture).
- [ ] Zero wire diff: `make protocol-ts-gen-check` + `make
      protocol-docs-gen-check` unchanged.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: N/A — none departed
