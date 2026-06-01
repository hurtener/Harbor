# Phase 108d — Console Live Runtime page (visual rebuild + dead-code cleanup)

> Fourth wave of the Phase 108 page-polish series. Brings the **Live Runtime**
> page to verbatim parity with its mock (`docs/rfc/assets/console-live-runtime-page.png`),
> every datum real-wired and verified live, governed by
> `docs/design/console/PAGE-POLISH-PROCEDURE.md` + `CONVENTIONS.md`. Live
> Runtime is the most complex page (topology graph, tab strip, event-stream
> dock, deep right rail, steering composer), so the implementer structures it in
> two stages for quality (Stage 1 non-graph, Stage 2 topology/tabs) — both in
> this one wave/branch/PR. Operator decisions (2026-05-31): **full rebuild to
> mock + cleanup**, and the topology graph is **verified structurally** (no flow
> runtime stood up — on the planner/runloop validation agent the canvas shows
> its honest info state; the graph RENDERING is verified via an injected sample
> `topology.snapshot` projection).

## Summary

The page's data layer already exists (Phase 73b / D-126): `topology.snapshot`,
`sessions.inspect`, `tasks.get`, `events.subscribe` (the 73b SSE fold benefits
from the 108c named-event fix), `pause.list` + `approve`/`reject`/`resume`, the
Phase 54 steering verbs (`start`/`redirect`/`inject_context`/`user_message`/
`cancel`/`pause`/`resume`), `llm.cost.recorded` aggregation. What diverges from
the mock is **presentation + layout + card consistency + empty/info states** —
the same class of work as 108c (Overview), at larger scale.

## RFC anchor

- **RFC §7** (Console), §7.1 runtime-lens, §6.3 (steering + pause/resume), §6.13
  (event bus). **page-live-runtime.md** is the authoritative per-page spec.
- **Decisions:** D-126 (Live Runtime composition), D-062 (Live Runtime ≠
  Sessions), D-065 (no session priority), D-066 (control-scope on
  Approve/Reject/Resume/Pause/hard-Cancel), D-072 (the ten control methods),
  D-164 (topology `unknown_method` → honest info state on planner/runloop
  runtimes), D-160 (disconnected predicate), D-026 (heavy-content), D-171
  (per-request session), plus the 108b chrome decision + 108c
  `PageState nested` / named-SSE-ingest fixes.

## Briefs informing this phase

- **Brief 11** §"Live Runtime view" (LR-1..LR-6), §PG-1..§PG-7 (chat is one
  panel), §CC-2 (identity-aware UI). **Brief 12** §"shared chat library".

## Findings I'm departing from / key constraints (verified live 2026-05-31)

1. **Topology graph + Timeline cannot render with live data on the validation
   (planner/runloop) agent.** `topology.snapshot` → `unknown_method` ("this
   Runtime hosts no engine — topology projection is not available") — verified
   live. On such runtimes the canvas shows the honest **info state** (D-164),
   which IS live-verified. Operator decision: **do NOT stand up a flow runtime**;
   the graph RENDERING (nodes/edges/legend/failed-node styling) is verified
   **structurally** via an injected sample `topology.snapshot` projection (unit/
   Playground-injected), documented as not-live-on-this-runtime in the ledger.
2. **Chrome supersedes the mock's per-page top elements** (search / notifications
   / runtime name+version+protocol) — those are the 108b app-shell chrome; the
   page does not rebuild them (see memory `feedback-console-chrome-supersedes-mock-topbar`).
3. **The mock's right-rail "Agent" / "User" / per-tenant fields** read from the
   identity tuple + `sessions.inspect`; the validation agent has no registered
   agent (`agent_id` empty) — Agent renders the connection/agent label or
   "—" (honest), not faked.

## Console consistency (CONVENTIONS.md §9 + PAGE-POLISH-PROCEDURE.md)

Routes at `/live-runtime` (+ `/live-runtime/[session_id]`) under `(console)/`;
renders in the app shell; `components/ui/` inventory + page-specific
`components/live-runtime/`; four-state `PageState` (page + nested per panel,
using the 108c `nested` prop); `HarborClient` + `connection.ts` only; teal
accent + Inter + carded panels (108b/108c); tokens only; D-160 disconnected;
D-066 control-scope gating on steering verbs.

## Implementation stages (one wave / branch / PR)

### Stage 1 — non-graph elements (live-verifiable on the validation agent)

Scope: everything that does NOT need the engine graph.

- **Status counter strip** — Pending/Running/Completed/Paused/Failed chips
  (colored). On planner runtimes derived from task/run states the page observes
  (event-fold) since `topology.snapshot` is absent.
- **Tab strip** — Topology | Timeline | Metrics | Health (styling + active
  state); Topology/Timeline render the info/empty state on this runtime.
- **Event-stream dock** (left) — `events.subscribe` table (Time · Type · event ·
  source); carded; row→detail.
- **Right rail** — Session detail card (Session ID, Status, Started, Duration,
  Tasks, Events, Agent, User, **Tenant**, **Cost**, **Last error**), Current
  Step, Recent Artifacts (mime + name + size + age), Interventions (cards +
  Approve/Reject/Resume, control-gated). All carded.
- **Composer** (bottom-right when no node selected) — Start / Redirect / Inject /
  User message / Cancel (soft/hard) / Pause / Resume — real verbs, scope-gated.
- **Bottom-dock layout** + per-task detail pane chrome (Details/Input/Output/
  Logs/Trace tabs) — wired to `tasks.get`/`state.history` where available.
- **Card consistency + empty/info states + dead-code cleanup.**

Acceptance: each datum real-wired + verified live; carded panels; four-state +
nested; zero console errors; tokens only; svelte-check 0/0; eslint no-unused
clean; §8 ledger for Stage 1; commit through preflight.

### Stage 2 — topology + timeline graph (structural verification)

Scope: the engine-graph centerpiece. NO flow runtime is stood up (operator
decision); the canvas shows the honest info state on the validation agent and
the graph rendering is verified structurally.

- Topology canvas to the mock: nodes (type tag + status pill + latency), edges,
  failed/reject terminal nodes (red border + code), status legend in the canvas
  corner, pan/zoom + reset-zoom + filter chips + pause-stream.
- Timeline tab (swimlanes), Metrics/Health tabs (probe `metrics.snapshot` /
  `runtime.health`; honest info state if absent).
- Verify the canvas STRUCTURALLY with an injected sample `topology.snapshot`
  projection (the page's `client` prop / a unit-rendered fixture), since the
  validation runtime returns `unknown_method`. Document in the §8 ledger that
  the graph is structurally-verified, not live-on-this-runtime.

## Per-datum source map (shipped vs flow-runtime-needed)

| Datum | Source | Checkpoint |
|---|---|---|
| Event stream | `events.subscribe` (named-SSE fix, 108c) | Stage 1 |
| Interventions | `pause.list` + approve/reject/resume | Stage 1 |
| Composer verbs | Phase 54 `start`/`redirect`/`inject_context`/`user_message`/`cancel`/`pause`/`resume` | Stage 1 |
| Session detail / Current Step | `sessions.inspect` | Stage 1 |
| Cost / Last error / Tenant | `llm.cost.recorded` / `task.failed`+`tool.failed`+`planner.error` / identity tuple (event-fold) | Stage 1 |
| Recent artifacts | `artifacts.list` (session-filtered) | Stage 1 |
| Per-task detail | `tasks.get` / `state.history` | Stage 1 |
| Status counters | task/run state fold (planner) / `topology.snapshot` nodes (engine) | Stage 1 (fold) / Stage 2 (graph) |
| Topology graph + legend | `topology.snapshot` (engine runtime; info state on planner) | Stage 2 (structural) |
| Timeline | `topology.snapshot` + task ordering | Stage 2 |
| Metrics / Health tabs | `metrics.snapshot` / `runtime.health` | Stage 2 |

## Dependencies / risks

- No new npm deps expected (topology canvas already exists). If a graph layout
  lib is needed it's an RFC §10 decision — current impl auto-lays-out already.
- Flow-runtime authoring (Stage 2) is the main new effort; the agent config lives
  under `~/harbor-validation/` (not committed).
- Cross-phase smoke drift (83r/83s-style) likely — fix in-wave (§17.6).

## Test plan

Per stage: unit (any new projection), rebuilt `live-runtime-page.spec.ts`,
`phase-108d.sh` smoke, live §3–§7, §8 ledger, §9 audit.

## Stage 1 ledger (verified live 2026-05-31)

Live env: validation agent :18080 + Console live source :18790, session `lr-demo`.

| Item | Result |
|---|---|
| Event-stream dock | **FIXED + verified** — `sub.open()` passed no eventTypes, so after the 108c named-SSE fix it received nothing; now lists the LR event vocabulary → 10+ rows stream live (task/tool/planner/cost). |
| Status counter strip | real (`tasks.list` aggregate + live deltas) — Completed (now) 5 observed |
| Topology canvas | honest **info state** (D-164) on the planner runtime — not a red error (verified) |
| Right rail (Session/User/Tenant/Agent/Status/Cost/Last error + Current Step + Recent Artifacts + Interventions) | carded (DetailRail/RailCard), real-wired via `sessions.inspect` + event-fold |
| Composer (Start/User message/Redirect/Inject/Cancel/Pause/Resume) | real Phase 54 verbs, scope-gated |
| Saved-view FilterBar strip | **removed** (mock has none) → replaced with header-row (counters + Refresh) + tab toolbar |
| Dead code | deleted `saved_filters_live_runtime.ts` + spec; dropped PageHeader/FilterBar/SavedViewChips imports + state + 4 fns + DB load |
| Console errors | 0 on clean load |

§17.6 cross-phase fixes: `phase-83s.sh` (live-runtime out of the save-view
label/placeholder loops) + `disconnected-state.spec.ts` (live-runtime out of the
N7 save-view list). Gates: svelte-check 0/0, lint clean, 83s smoke 24 OK,
live-runtime + disconnected e2e 18/18.

## Acceptance criteria

- **Stage 1:** each datum real-wired + verified live; carded panels; four-state
  `PageState` (page + nested per panel); zero console errors on clean load; tokens
  only (stylelint); `svelte-check --fail-on-warnings` 0/0; eslint no-unused clean.
- **Stage 2:** topology canvas + Timeline + Metrics/Health tabs to the mock; the
  graph rendering verified **structurally** via an injected sample
  `topology.snapshot` projection (the validation runtime returns `unknown_method`,
  D-164 honest info state live); topology is **not** the default-selected tab
  (runtime-conditional — default to an always-meaningful tab / default-by-capability).
- **Both stages:** `scripts/smoke/phase-108d.sh` shows OK ≥ the assertions it
  covers and FAIL = 0; prior-phase smokes still pass; §8 per-datum ledger present;
  §9 read-only checkpoint audit FAIL-free; deleted code has zero references.

## Files added or changed

- `web/console/src/routes/(console)/live-runtime/+page.svelte` — rebuilt to the mock.
- `scripts/smoke/phase-108d.sh` — new (static-only smoke for this wave).
- `scripts/smoke/phase-83s.sh` — cross-phase §17.6 (live-runtime out of the
  saved-view label/placeholder loops).
- `web/console/tests/disconnected-state.spec.ts` — live-runtime out of the N7
  saved-view list; `web/console/tests/live-runtime-page.spec.ts` — rebuilt.
- Deleted: `web/console/src/lib/db/saved_filters_live_runtime.ts` + its spec
  (orphaned saved-filters store, dead after the FilterBar strip removal).
- `docs/plans/phase-108d-console-live-runtime-page.md` — this plan.

## Smoke script additions

- `scripts/smoke/phase-108d.sh` (static-only, `PREFLIGHT_REQUIRES: static-only`):
  asserts the `FilterBar` / `SavedViewChips` imports and the `live-runtime-save-view`
  testid are gone; the event stream subscribes to **named** event types
  (`task.completed`, `llm.cost.recorded` — the 108c named-SSE fix); `TabStrip` +
  `live-runtime-refresh` present; the D-164 topology info state ("Topology view not
  available") present; the dead saved-filters store deleted. Greps anchor on
  imports / testids, never bare strings that match prose comments.
- `scripts/smoke/phase-83s.sh`: drops `live-runtime` from the saved-view
  label/placeholder loops (it keeps the page in the N2 footer-dedup loop).

## Coverage target

Console-only wave — no new Go/Protocol surface, so no Go coverage delta. The gate is
`svelte-check --fail-on-warnings` 0/0 + eslint no-unused clean + stylelint
tokens-only. Behavioural coverage lives in `web/console/tests/live-runtime-page.spec.ts`
(rebuilt) and `web/console/tests/disconnected-state.spec.ts`; the topology graph
(Stage 2) is verified structurally there via an injected `topology.snapshot`
projection, since the planner/runloop validation runtime returns `unknown_method`.

## Pre-merge checklist (per stage)

- [ ] svelte-check 0/0 · eslint no-unused clean · lint (tokens)
- [ ] live-verified (zero console errors) · four-state + nested
- [ ] drift-audit + phase smoke + markdownlint + check-mirror
- [ ] §8 ledger · §9 audit FAIL-free · deleted code has zero refs
