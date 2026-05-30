# Phase 108a — Playground fidelity pass (follow-up to D-167)

<!-- markdownlint-disable-file MD029 -->
<!-- This checklist numbers its items GLOBALLY (1..N) across the ### section
     headers and cross-references them by that number ("item 19", "item 1"),
     so the ordered-list prefixes intentionally do not restart per section.
     MD029/ol-prefix is disabled file-wide to preserve that numbering. -->

## Summary

Phase 108 (D-167) made the Playground functional — fixed-viewport shell, real
streaming, real KPIs from `llm.cost.recorded`, live Events-Stream indicator,
markdown rendering. Phase 108a closes the **visual-fidelity and depth gap**
against the binding mockup `docs/rfc/assets/console-playground-page.png`: the
top header still wastes vertical space and trails the mock's compact two-row
band; the KPI strip recovered function but is four boxed tiles instead of the
mock's integrated metadata row (and is missing Session/Started/Duration/
Identity/Scope); the chat bubbles and especially the composer are far simpler
than the mock (no per-message meta, no action icons, no rich composer button
cluster, no composer telemetry row); the bottom status bar was scoped to the
Playground page when the mock has **one global app-shell status bar**; and the
right rail's Controls card carries an "Apply to next message" button and a
"Post-V1" drift toggle that contradict the "controls re-wire the next request
live" intent and the "no Post-V1 labels above V1" principle.

Phase 108a is **Console-only and additive** to the Protocol surface — no new
wire field, method, or runtime emit. Where the mock shows data the Protocol
does not currently expose (per-message token/cost attribution, context-window
size, runtime-side reasoning traces), the gap is named explicitly and either
derived Console-side, deferred with a visible honest state, or flagged as a
**runtime-side** dependency (it is NOT papered over with synthetic data —
CLAUDE.md §13).

## RFC anchor

- RFC §7 — Console as Protocol client (the Playground is the first-touch
  operator surface; visual polish is the adoption gate, RFC §1).
- CONVENTIONS.md §2/§3/§7 — app shell, `ui/` inventory, one token scale.

## Findings that shape this phase (verified live, 2026-05-29)

- **F1 — Agent/runtime display name IS available Console-side.** The Settings
  "Connected Runtimes" form persists a name via `db.addRuntime(name, url, …)`
  into the Console DB address book (`db.runtimes`, rendered as `rt.name` in
  `settings/+page.svelte`). The dev runtime's Agent Registry is empty and
  `parent_session.agent_name` is blank, so the Phase 108 fallback landed on
  "default agent". 108a resolves the name from the address book by matching the
  active connection's `base_url`. (Answers the operator's question directly.)
- **F2 — Reasoning/thinking is a RUNTIME gap, not a Console gap.** With
  `reasoning_effort: medium` on `anthropic/claude-haiku-4.5`, a live `harbor dev`
  SSE capture shows **only `content` chunks (0 `reasoning` chunks)**,
  `planner.decision.ReasoningChars=0` with an empty trace, and
  `tasks.get.trajectory == null` (the dev posture does not wire the trajectory
  enricher). Probable cause: `json_schema_mode: native` is mutually exclusive
  with extended thinking on this provider. **No Console change surfaces thinking
  the runtime never emits.** 108a ships the Console-side rendering (reasoning
  chunk accumulation + accordion) so it lights up the moment the runtime emits
  reasoning; the runtime fix is tracked separately (see §"Runtime dependencies").
- **F3 — `tasks.get.cost` rollup is always 0 for dev foreground turns;** real
  tokens/cost are on the `llm.cost.recorded` event (already wired in 108). Any
  per-message cost/token attribution in 108a reads from that event, not the
  task detail.
- **F4 — Context-window size is not on the Protocol surface.** `runtime.info`
  carries no model context-window; `llm.cost.recorded` carries usage but not the
  window. The mock's "Context: 18.2k / 128k (14%)" needs the window size. 108a
  shows absolute context tokens (derivable) and renders the `/window (%)` only
  when a window size is known; otherwise it omits the percentage (no fabricated
  128k). Exposing the window is a runtime follow-up.

## Goals

Reach the mockup's composition and density on every region of the Playground,
plus promote the status bar to a single global app-shell surface, without
adding a Protocol method, a runtime emit, or an npm dependency, and without any
"Post-V1" label on a surface that should simply ship.

## Non-goals

- No new Protocol method / wire field / runtime emit.
- No global `⌘K` command palette (separate phase; the mock's top-right search is
  explicitly out of scope here).
- No new npm dependency (AC carryover from 108 / §13).
- No runtime-side reasoning-emission fix (tracked as a dependency, not built here).

---

## Ordered change list

Each item: **what changes · files · expectation · mock ref · data source/caveat.**
Implementation proceeds in this order (foundation → regions → cross-cutting →
verify).

### A. Top header compaction — mock Image 3 vs current Image 4

1. **Collapse the oversized `PageHeader` title block into a compact two-row
   band.** Remove the large "Playground" H1 + "Session chat · steering ·
   overrides" subtitle as the prime element; the page identity moves into the
   shell breadcrumb line as `Playground / <session-id>` with the run-status pill
   (`Active`/`Ready`/`Streaming`) inline, matching the mock's row 1.
   - Files: `routes/(console)/playground/[session_id]/+page.svelte`,
     `components/playground/Header.svelte` (and possibly a slimmer use of the
     shared `PageHeader`).
   - Expectation: row 1 reads `Playground / sess_…  [Active]` like Image 3, not a
     tall title + subtitle stack.
2. **Agent sub-bar (row 2) with leading-icon pills.** `Agent: ● <name> ·
   Model: <model> ↻ · Planner: <planner>` on the left; action cluster on the
   right. Pills get small leading glyphs and are visually lighter (label +
   value), matching Image 3.
   - Files: `Header.svelte`.
   - Expectation: a single tight row, not the current wrapped chips.
3. **Action cluster: `Pause` · `Cancel run` · `Redirect ▾` · overflow `⋮`.**
   Replace the current `Cancel run` + `Restart` pair. `Pause`/`Resume` toggles
   the run (SHIPPED `pause`/`resume` control verbs); `Cancel run` stays (danger);
   `Redirect ▾` issues a steering redirect (SHIPPED `user_message`/redirect path)
   or is disabled-with-tooltip when no run is in flight; `Restart` moves under the
   overflow `⋮`.
   - Files: `Header.svelte`, `+page.svelte` (wire pause/resume/redirect handlers).
   - Expectation: button row matches Image 3; every action maps to a SHIPPED verb;
     no faked buttons.
   - Caveat: `Redirect` uses the existing steering inject; if the active planner
     can't redirect mid-run it renders disabled-with-tooltip (CONVENTIONS.md §5),
     never a dead button.
4. **Reclaim the `FilterBar` row.** The "No saved views / Save current as / Save
   view / Filter messages" row (Image 4) is not in the mock's prime band. Move
   saved-views + message-filter behind the header overflow `⋮` menu (or a single
   compact "Views" affordance). The feature is preserved (D-061), not deleted —
   it just stops consuming a full row above the chat.
   - Files: `+page.svelte`.
   - Expectation: the chat/KPI band starts directly under the agent sub-bar; no
     standalone filter row.

### B. KPI metadata strip — mock Image 5 vs current Image 6

5. **Convert the four boxed tiles into one integrated metadata row** with subtle
   vertical dividers (no per-tile borders/background), bold values, small
   grey uppercase labels above — exactly the Image 5 treatment.
   - Files: `components/playground/KpiStrip.svelte`.
   - Expectation: a single hairline-bordered band, columns separated by dividers,
     visually identical density to Image 5.
6. **Add the mock's columns: Session ID (copy icon) · Started · Duration ·
   Tokens (in/out) · Cost (ceiling bar) · Identity · Scope.**
   - Session ID: from the URL/connection (copyable — already in Header in 108;
     move/echo here per mock).
   - Started: timestamp of the session's first turn (tracked Console-side on first
     send; `Intl.DateTimeFormat`).
   - Duration: live elapsed since Started (`mm:ss`/`Hh Mm`), ticking while active.
   - Tokens / Cost: reuse the 108 wiring (`llm.cost.recorded`), keep in/out +
     ceiling bar.
   - Identity: `connection.identity.user` (e.g. `dev` / `you@…`).
   - Scope: `Tenant: <tenant> / <role>` from `connection.identity.tenant` + scopes.
   - Files: `KpiStrip.svelte`, `+page.svelte` (Started/Duration tracking).
   - Expectation: 7-column metadata row per Image 5.
   - Caveat: **Latency/Status do not appear in the mock's KPI row** — Status moves
     to the header pill (item 1); p50 Latency is dropped from this row to match the
     mock (the data stays available; it can return as a header tooltip — flagged as
     a minor judgment call to confirm).
7. **Move the impersonation "Run as" control out of the header into the
   Identity/Scope columns as the `▾` dropdowns** the mock shows.
   - Files: `KpiStrip.svelte` (or a small `IdentityScopePicker`), `+page.svelte`.
   - Expectation: Identity and Scope are dropdowns (admin) / static (non-admin),
     matching Image 5; the header no longer carries "RUN AS".
8. **Remove the "Topology view not available on this Runtime" text from the
   Status surface.** It already degrades elsewhere; it should not occupy KPI
   real estate (Image 6 clutter). Topology availability stays surfaced via the
   Trace card's existing info state.
   - Files: `KpiStrip.svelte`, `+page.svelte`.

### C. Chat bubble depth — mock Image 7

9. **Bubble header: real agent name + per-message meta + action icons.** Show
   the resolved agent name (item 19), the timestamp, a `Reasoning` chip when the
   message carries reasoning, and right-aligned `copy` / `thumbs-up` /
   `thumbs-down` icon buttons. Add a per-message meta line `<elapsed> · <tokens>
   [· $<cost>]` from the turn's `llm.cost.recorded` + lifecycle timing.
   - Files: `chat/MessageBubble.svelte`, `+page.svelte` (attribute the turn's
     cost/tokens/elapsed to the agent message).
   - Expectation: bubble head matches Image 7 (name · time · Reasoning chip ·
     action icons; footer meta with timing/tokens/cost).
   - Caveat: copy works client-side; thumbs up/down are local feedback only (no
     Protocol feedback surface) — rendered, but stored Console-locally or marked
     plainly as local until a feedback method exists (no fake server call).
10. **Reasoning summary accordion.** Render the existing `ReasoningAccordion`
    with the mock's "Reasoning summary (N steps) · <elapsed> · <tokens>" header
    and collapsed body. Populate from (a) `reasoning`-kind stream chunks
    accumulated during the turn, and (b) `tasks.get.trajectory.steps` when present.
    - Files: `MessageBubble.svelte`, `chat/ReasoningAccordion.svelte`,
      `+page.svelte` (accumulate `kind:'reasoning'` chunks — currently dropped).
    - Expectation: when reasoning exists, the accordion renders like Image 7;
      when it does not, nothing renders (no empty box).
    - Caveat: **gated on F2** — with today's dev runtime it stays empty because no
      reasoning is emitted. Console code ships ready; it lights up when the runtime
      emits reasoning.
11. **Tool-call trace rows.** Render the mock's tool rows (status dot · tool name
    · server tag · query · timestamp · duration) and the `N tool calls · <total>
    · View raw trace` footer, fed by `tool.*` / `planner.decision` events captured
    during the turn.
    - Files: `chat/ToolCallTraceCard.svelte` (exists — extend), `+page.svelte`
      (subscribe to `tool.invoked`/`tool.completed`/`planner.decision`, attribute
      to the message).
    - Expectation: tool-using turns render the Image 7 tool block; `View raw trace`
      links to the existing trace surface.
    - Caveat: requires subscribing to tool/planner events and attributing them to
      the in-flight task; the youtube agent exercises this (its MCP tools).
12. **Avatar polish.** Agent avatar uses the Harbor mark (not a bare "A"); user
    avatar keeps initials. Subtle role tint per the chip palette (Image 7).
    - Files: `MessageBubble.svelte`.

### D. Composer richness — mock Image 7/8 vs current Image 8

13. **Composer container + drop zone.** A single rounded recessed container: a
    full-width textarea on top, and a bottom action row. Show the
    "⊙ Drag & drop files here or click to upload" affordance as the mock does
    (the drop target already exists via `uploadArtifact`; surface it visually).
    - Files: `chat/ChatComposer.svelte`.
    - Expectation: matches Image 7 composer framing; darker recess + hairline
      divider between the attach control and the textarea.
14. **Bottom action cluster (left→right): attach (paperclip) · web-search toggle
    (globe) · code toggle (`</>`) · settings (gear) · primary Send (→, accent) +
    split `▾` send-mode dropdown.** The split dropdown replaces the current
    radio "Queue / Steer" fieldset — `Queue` vs `Steer` become the `▾` menu on the
    Send button (mock Image 7).
    - Files: `ChatComposer.svelte`.
    - Expectation: button order + grouping matches Image 7; Send is the accent
      arrow button with a mode dropdown.
    - Caveat: globe (web search) and `</>` (code/raw input) toggles map to real
      behaviours only if supported — web-search is **not** a shipped capability,
      so it renders disabled-with-tooltip (no faked toggle); `</>` toggles a
      raw/markdown input mode (client-side, real). Voice (mic) stays as today
      (disabled-with-tooltip when no SpeechRecognition).
15. **Composer telemetry row (page-specific live metrics).** Under the composer,
    the mock's thin row: `● Streaming · Tokens/sec: <n> · Context: <used>[ / <win>
    (<pct>)] · Cost: $<spend> / $<ceiling> (<pct>) ▬ · Session live ●`.
    - Files: `ChatComposer.svelte` (or a small `ComposerStatus` slot fed by the
      page).
    - Expectation: matches the Image 7 bottom strip.
    - Data: Streaming + Session-live from stream state; Tokens/sec computed from
      `content` deltas over elapsed; Cost/ceiling from 108 wiring; **Context
      window % only when the window size is known (F4)** — else show absolute used
      tokens without the `/win (%)`.
16. **Replace the bare "~N tokens" foot** with the telemetry row above; keep a
    minimal token estimate inside it.

### E. Global app status bar — mock Image 9

17. **Unify `PlaygroundStatusBar` + `ConnectionFooter` into ONE global shell
    status bar rendered on every page** (not Playground-only). Left:
    `● Connected to <runtime name>` (F1 name, falls back to base URL). Center:
    `Protocol <version>` + `Events Stream: Live/Off ●`. Right: `Console <version>`.
    - Files: `routes/(console)/+layout.svelte`, a new
      `components/ui/AppStatusBar.svelte` (replaces both `ConnectionFooter` usage
      and the page-scoped status-bar slot); delete/retire the page `status-bar`
      slot plumbing from 108 and `PlaygroundStatusBar.svelte`.
    - Expectation: every Console page shows the single Image 9 bar; the page no
      longer injects a status bar.
    - Data: connection + name shell-side (`connection.ts` + address-book lookup);
      Protocol version via a one-shot shell `runtime.info`; **Events-Stream-Live
      via a shared module-level liveness store** any page's EventSource updates
      (the Playground sets it; pages without a stream show `Off`). Console version
      from `import.meta.env.VITE_CONSOLE_VERSION ?? 'dev'`.
    - Caveat: making Events-Stream-Live truly global without a shell-owned
      subscription uses the shared store; documented so it is not mistaken for a
      shell-level heartbeat.
18. **Remove the now-duplicated page status bar + the 108 `statusBar` Snippet/
    context** from `+page.svelte` and the shell, since E folds it into the shell.

### F. Right rail — mock Image 10 vs current Image 11

19. **Resolve and show the real runtime/agent name** (F1) across Header (item 2),
    bubbles (item 9), and the global bar (item 17). Resolution chain:
    address-book `rt.name` for the active `base_url` → `agents.list[0].name` →
    `runtime.info.display_name` → `default agent`.
    - Files: `connection.ts` (or a small resolver) + consumers; `+page.svelte`.
    - Expectation: the name the operator typed in Settings shows everywhere a name
      is shown.
20. **Controls: remove "Apply to next message"; apply overrides live.** Changing
    a control writes through `runs.set_overrides` immediately (debounced), with a
    small inline "applies to your next message" hint and a subtle saved/echo
    indicator — no modal save button (Image 10 has none).
    - Files: `components/playground/ControlsCard.svelte`, `+page.svelte`.
    - Expectation: editing a control takes effect with no extra click; Image 10
      layout (no big button).
21. **Controls: show real Temperature / Top P values (not "—").** Initialize the
    sliders to the effective defaults and bind the numeric value so the chip always
    reflects the thumb (Image 11 shows "—" with a mid-thumb — inconsistent).
    - Files: `ControlsCard.svelte`.
22. **Controls: add `Model` and `Tools` dropdowns; add `Reset to defaults`.**
    `Model` lists available models (or shows the active one read-only if the
    runtime exposes no model list); `Tools` shows `All enabled (N)` from
    `agents.tools`/`tools.list` with per-tool enable toggles; header gets a
    `Reset to defaults` link (Image 10).
    - Files: `ControlsCard.svelte`, `+page.svelte`.
    - Caveat: if the runtime exposes no model switch, `Model` is read-only (honest)
      rather than a fake selector.
23. **Controls: remove the "Drift mode / Post-V1" toggle entirely.** Do not show a
    disabled control labelled Post-V1 (operator's principle: above V1, ship it or
    hide it). Drift is not a shipped runtime capability, so it is removed from the
    V1 Controls; it returns when the runtime supports it.
    - Files: `ControlsCard.svelte`.
24. **Controls: System-prompt override as a compact `Off` toggle + lock that
    expands to the textarea** (Image 10), instead of an always-open textarea.
    - Files: `ControlsCard.svelte`.
25. **Pending Interventions card: coloured intent treatment + Connect action.**
    Amber count badge; per-row intent chip coloured by source event
    (`tool.approval_requested` → warning, `tool.auth_required` → accent/OAuth,
    `pause.requested` → danger); Tool/Target/Reason fields; `Approve` (success) /
    `Reject` (danger) and, for OAuth rows, `Connect` (accent) (Image 10).
    - Files: `components/playground/PendingInterventionsCard.svelte`, `+page.svelte`
      (populate interventions from the existing pause/approval events — currently
      the array is never filled).
    - Expectation: matches Image 10; the youtube agent's approval/OAuth flows feed
      real rows.
    - Caveat: requires subscribing to the pause/approval events and mapping them;
      the intent→colour map follows the 108 decision #3 (source-derived, no `type`
      field).
26. **Recent Artifacts card: kind icon · name · kind · time · size · `View all`.**
    Populate from `artifacts.list` for the session (currently the array is never
    filled), file-type icon from the renderer registry's `mimeIcon`, age via
    `Intl.RelativeTimeFormat`, `View all` → `/artifacts` (Image 10).
    - Files: `components/playground/PlaygroundArtifactsCard.svelte`, `+page.svelte`.
    - Expectation: matches Image 10; rows reflect real session artifacts.
27. **Rail density + visibility.** With A/E reclaiming top and bottom space, ensure
    all three cards (Controls / Pending Interventions / Recent Artifacts) are
    visible/scrollable per Image 10's compact stack (Image 11 showed only one card).
    - Files: `+page.svelte` layout, `DetailRail` usage.

### H. Session lifecycle & DX — operator-reported friction

Context (verified): the dev token pins `session=dev` (the JWT's session claim);
`control.start` folds identity from the token, so every write lands in `dev`.
There is **no `sessions.create`** Protocol method (only `sessions.list` +
`sessions.inspect`). `harbor dev` boot calls `sessionRegistry.Open("dev")`
(`cmd_dev.go:1085`); when the persisted SQLite state has `dev` closed
(idle-GC'd), `Open` returns "reopen-after-close forbidden" and **boot fails** —
the exact friction hit when restarting after an agent-code change.

28. **`harbor dev` survives restart when the default session was idle-GC-closed
    (RUNTIME — highest priority, do first).** Boot must reopen/revive or
    re-provision the dev session instead of crashing, so iterating on agent code
    (edit → restart → continue) never requires manually deleting the state DB.
    - Files: `cmd/harbor/cmd_dev.go` (the `Open(DevSession)` bootstrap), possibly
      `internal/sessions` (a dev-permitted reopen/revive path).
    - Expectation: `harbor dev` restart on an existing state DB always boots 200;
      no "reopen-after-close forbidden" crash.
    - Tag: RUNTIME (tracked as R4). This is the DX blocker — prioritise it.
29. **`/playground` (no id) resolves to the active session (Console).** The sidebar
    entry and a bare `/playground` route to the operator's current session
    (`connection.identity.session` in dev) instead of requiring a hand-typed id.
    - Files: `routes/(console)/playground/+page.(svelte|ts)` (redirect),
      sidebar entry in `(console)/+layout.svelte`.
30. **Session switcher + "New session" in the Playground header (Console).** A
    session picker fed by `sessions.list` (scoped reads are allowed by the dev
    token's `admin`/`console:fleet`); "New session" starts a fresh conversation.
    - Files: `Header.svelte` (or a `SessionSwitcher`), `+page.svelte`.
    - Caveat: a *writable* new session needs a token carrying that session claim —
      see R5. Until R5, "New session" is gated/labelled honestly (it cannot mint a
      writable session in the single-token dev posture); the switcher's **open a
      past session** path works now (read-only, item 31).
31. **Open a previous session to view its behaviour (Console).** Selecting a past
    session loads its history read-only via `sessions.inspect` + the session's
    `tasks.list` (each task's `result_inline`/trajectory) so the operator can
    compare behaviour across agent-code changes — the operator's core ask.
    - Files: `+page.svelte`, `Header.svelte`/`SessionSwitcher`.
    - Data: `sessions.list` → pick → `sessions.inspect` + `tasks.list(session)` →
      hydrate `messages` read-only.
32. **Hydrate prior turns on session open (Console).** Opening any session (current
    or past) loads its completed turns into the stream (from `tasks.list` for the
    session) instead of showing an empty chat, so reopening shows the conversation.
    - Files: `+page.svelte` (load() extension).
    - Caveat: depends on the runtime exposing per-session task history to the
      Console (verify `tasks.list` filters by session under the dev token).

### G. Verification & tests

33. **Verify overrides actually take effect end-to-end.** Drive `runs.set_overrides`
    (e.g. temperature 0 vs 2, reasoning effort) against the live youtube runtime and
    confirm the next turn reflects it (answers the operator's "does it work?").
    Capture in an e2e assertion where deterministic.
34. **Unit/logic tests (dep-free):** Started/Duration formatting; agent-name
    resolution chain (F1); tokens/sec + context computations; intent→colour map;
    interventions/artifacts population from decoded events. Extend
    `wire-events.ts` decoders for `tool.*` / pause/approval / artifact events with
    a spec.
35. **e2e updates:** update `playground-polish.spec.ts` / `playground-page.spec.ts`
    for the new header actions, integrated KPI row, composer cluster, global status
    bar, live-apply Controls, and the session switcher / past-session view;
    `shell-no-regression.spec.ts` must still pass with the global status bar on all
    13 other pages. Add a `harbor dev` restart-resilience smoke (item 28 / R4).
36. **Gates:** `npm run check` (0/0), `npm run lint` (no raw literals), `npm run
    test`, `npx playwright test`, `scripts/smoke/phase-108.sh` (extend or add
    `phase-108a.sh` for the global-status-bar + no-Post-V1 + restart-resilience
    assertions), no new npm dependency.

---

## Runtime dependencies (NOT built in 108a — tracked here)

- **R1 — Reasoning emission (F2).** The runtime/LLM edge must emit `reasoning`
  stream chunks and/or populate `tasks.get.trajectory` for reasoning-capable
  models; today `json_schema_mode: native` appears to suppress thinking on
  `anthropic/claude-haiku-4.5`. Console reasoning UI (items 9/10) ships ready and
  lights up once this lands. Owner: runtime LLM driver / planner phase.
- **R2 — Context-window size on the Protocol (F4).** Expose the active model's
  context-window (e.g. on `runtime.info` or `llm.cost.recorded`) so the composer
  telemetry can show `used / window (%)`. Until then 108a shows absolute context
  tokens only.
- **R3 — Model/tool switch + feedback surfaces.** Live model switch, per-tool
  enable, and message feedback (thumbs) have no Protocol method today; 108a renders
  read-only/local-only and does not fake server calls.
- **R4 — `harbor dev` restart resilience (item 28, HIGHEST priority).** Boot must
  not crash on a persisted-closed default session. This is the active DX blocker
  (edit agent → restart → "reopen-after-close forbidden"). Small, contained
  `cmd_dev.go` / `internal/sessions` fix; it can land first, even ahead of the
  visual work.
- **R5 — Writable multi-session in the dev posture.** The dev token pins one
  session and there is no `sessions.create`; auto-generating a *writable* new
  session per conversation (item 30) needs either per-session dev-token minting
  (a dev-only "new session → new token" surface) or a session-override on the
  control path under the admin/`console:fleet` scope. Until R5, the Console can
  *view* past sessions (items 31/32) but cannot *start* a second writable one.

## Console consistency (CONVENTIONS.md §9)

Unchanged routing/shell/`ui/`/`PageState`/`HarborClient`/tokens posture. The one
structural change is promoting the status bar to a shared `ui/AppStatusBar`
rendered by the shell on every page (replaces `ConnectionFooter` + the 108 page
status-bar slot) — additive to the shell, no page regressions (item 30).

## Resolved decisions (operator, 2026-05-29)

- **D-Q1 — Keep p50 Latency** as a column in the KPI row (item 6) — it is NOT
  dropped; the row carries the mock's columns plus Latency.
- **D-Q2 — Omit the web-search globe** entirely until web search actually ships
  (item 14) — do not render a disabled placeholder.
- **D-Q3 — Remove the Drift-mode toggle entirely** (item 23). No "Post-V1" labels.
- **D-Q4 — Token = per-backend connection credential (API-key-like), NOT a
  session pin.** The token authenticates the Console↔backend connection and
  carries `tenant + user + scopes`; **session is dynamic and per-conversation**.
  One token operates all of that backend's sessions; the Console address book
  holds many backends (each with its own token). Future console login + RBAC
  decides which backends a user reaches. This makes the Phase-108 "session in the
  JWT" a **miss**; 108a corrects it: session is supplied per-request and created
  on first use (see §H + R4/R5, now folded into one runtime rework).
- **D-Q5 — Fix the session model in THIS PR**, driven by a subagent (audited by
  the coordinator — the completion signal is not trusted; the coordinator
  independently re-runs the live proof and reads the isolation tests).

## Thinking validation result (F2 → confirmed wiring bug)

Switching the test agent to `openai/gpt-5.4` (a reasoning model) at
`reasoning_effort: medium` STILL emits zero reasoning (233 content chunks, 0
reasoning chunks, `ReasoningChars=0`, `Usage.ReasoningTokens=0`). Reproduced on
two models ⇒ **R1 is a Harbor wiring bug, not a model limitation.** A subagent is
diagnosing it the way the 107c bug was found: live-prove OpenRouter/bifrost
reasoning directly via the `.env`, then trace Harbor's bifrost driver → planner →
event emit, fix the break, and prove reasoning chunks emit live. Coordinator
audits the root cause + re-runs the live proof.

## Briefs informing this phase

- brief 11 — Console feature surface (the binding Playground inventory, §PG-1..§PG-7).
- brief 12 — Console deployment + shared UI library (the shared `web/console/src/lib/chat/` module, D-091).
- brief 13 — react prompt engineering / operator UX (perceived polish dominates first impressions).

## Acceptance criteria

The binding acceptance criteria for 108a ARE the numbered items in the
"Ordered change list" above (items 1–36) plus the runtime dependencies
(R1–R5). A change is done when its item's expectation holds, `npm run
check`/`lint`/`test` and the Playwright specs are green, and
`scripts/smoke/phase-108a.sh` shows the matching OK with FAIL=0.

## Files added or changed

- `web/console/src/lib/components/ui/AppStatusBar.svelte` (NEW — global bar, items 16–18).
- `web/console/src/lib/components/playground/{KpiStrip,Header,ControlsCard}.svelte` (items A/B/F).
- `web/console/src/lib/components/playground/PlaygroundStatusBar.svelte` (REMOVED — folded into AppStatusBar).
- `web/console/src/lib/chat/{MessageBubble,ChatPanel,types.ts,markdown-parser.ts}` (items C, markdown fix).
- `web/console/src/routes/(console)/+layout.svelte` (shell renders the global bar).
- `web/console/src/routes/(console)/playground/[session_id]/{+page.svelte,chunk-stream.ts,wire-events.ts,wire-events.spec.ts}`.
- `internal/llm/{corrections,output,retry}` + `internal/llm/llm.go` (R1 reasoning wiring fix).
- `internal/protocol/auth/middleware.go`, `internal/sessions/*`, `internal/protocol/*`, `cmd/harbor/cmd_dev.go`,
  `harbortest/devstack` (R4/R5 session model — D-171).
- `scripts/smoke/phase-108a.sh` (NEW), `docs/notes/session-model-contract.md` (NEW).

## Test plan

- Unit (Vitest): `wire-events.spec.ts` decoders; existing chat/markdown specs.
- Go unit (`-race`): `internal/llm/...`; `internal/sessions/...` + `internal/protocol/auth/...`;
  `test/integration/session_model_d171_test.go`.
- e2e (Playwright): `playground-page` / `playground-polish` / `shell-no-regression`.
- Live proof: reasoning chunks 0→>0; multi-session create/list/reload + restart-resilience over SQLite.

## Smoke script additions

`scripts/smoke/phase-108a.sh` (`PREFLIGHT_REQUIRES: static-only`) — asserts the global
AppStatusBar landed, the page-scoped status bar removed, the KPI integrated columns, Controls
live-apply (no save button / no Post-V1 drift), the composer telemetry + reasoning render, the
corrections empty-Model default, and no-new-deps.

## Coverage target

- `web/console/src/lib/chat/`: 85% (existing). `web/console/src/lib/components/playground/`: 80%.
- `internal/llm/...`: unchanged package targets; the reasoning fix adds tests.

## Dependencies

- 108 (D-167) — the page/shell foundation this pass refines.
- 73n / 105 / 106 — Playground foundation + first-attach + real answer (shipped).
- D-121 / CONVENTIONS.md — the Console design-system foundation.
- D-171 — the corrected session/identity model (this pass, runtime side).
