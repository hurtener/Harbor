<script lang="ts">
  // Harbor Console — Playground page (`/playground/<session_id>`),
  // Phase 73n / D-130. Built on the D-121 design-system foundation
  // (CONVENTIONS.md) and the shared chat module (D-091).
  //
  // The Playground is a real Harbor session: every message round-trips
  // through the SHIPPED `user_message` Protocol method (Phase 54) — NO
  // parallel chat protocol. The page composes:
  //   - the shared chat module (`$lib/chat/`, D-091) — `<ChatPanel>` is
  //     the FIRST consumer; the page injects a `ChatProtocolClient`
  //     adapter over the Console `HarborClient` (the chat module never
  //     constructs a client, never reads `connection.ts`).
  //   - the four-state `<PageState>` async contract (CONVENTIONS.md §4).
  //   - the shared `ui/` inventory: `PageHeader`, `FilterBar`,
  //     `SavedViewChips`, `DetailRail`/`RailCard`, `Pagination`,
  //     `ConnectionFooter`, `PageState`.
  //   - the unified `HarborClient` + `connection.ts` (CONVENTIONS.md §6):
  //     `runs.set_overrides`, the SHIPPED Phase 54 control verbs
  //     (`user_message` / `cancel` / `start` / `approve` / `reject`),
  //     `artifacts.put` / `artifacts.get_ref`, `topology.snapshot`.
  //   - Console-DB-backed `SavedViewChips` (D-061) — Controls-card
  //     override presets via `PlaygroundSavedFilters`.
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).
  import { onDestroy, onMount } from 'svelte';
  import { page } from '$app/state';
  import SavedViewChips, { type SavedView } from '$lib/components/ui/SavedViewChips.svelte';
  import DetailRail from '$lib/components/ui/DetailRail.svelte';
  import RailCard from '$lib/components/ui/RailCard.svelte';
  import Pagination from '$lib/components/ui/Pagination.svelte';
  // The global status bar (connection + protocol + events + console) is
  // rendered ONCE by the app shell ((console)/+layout.svelte — 108a).
  import PageState, { type PageStatus } from '$lib/components/ui/PageState.svelte';
  import PlaygroundHeader, {
    type ImpersonationTarget
  } from '$lib/components/playground/Header.svelte';
  import KpiStrip from '$lib/components/playground/KpiStrip.svelte';
  import ControlsCard from '$lib/components/playground/ControlsCard.svelte';
  import PendingInterventionsCard, {
    type PendingIntervention
  } from '$lib/components/playground/PendingInterventionsCard.svelte';
  import PlaygroundArtifactsCard, {
    type RecentArtifactEntry
  } from '$lib/components/playground/PlaygroundArtifactsCard.svelte';
  import TraceToggle, { type TraceNode } from '$lib/components/playground/TraceToggle.svelte';
  import { ChatPanel, type ChatMessage, type ChatProtocolClient } from '$lib/chat/index.js';
  import { MAX_SESSION_TITLE_LEN } from '$lib/sessions/types.js';
  import {
    chatCatalogListRequest,
    SessionsProtocol
  } from '$lib/protocol/sessions.js';
  import AppPanel from '$lib/components/playground/AppPanel.svelte';
  import AppTabStrip from '$lib/components/playground/AppTabStrip.svelte';
  import SplitPane from '$lib/components/playground/SplitPane.svelte';
  import {
    reduceLayout,
    computeRegion,
    INITIAL_LAYOUT,
    appId,
    type LayoutModel,
    type LayoutAction,
    type DisplayMode,
    type AppRef,
    type OpenApp
  } from '$lib/components/playground/layout.js';
  import { makeMCPAppHostClient } from '$lib/mcp-app-host-client.js';
  import type {
    DisplayModeRequest,
    McpUiDisplayMode,
    MCPAppHostClient,
    MCPAppRefView
  } from '$lib/chat/renderers/app-bridge-host.js';
  import { HarborClient, type ProtocolClient } from '$lib/protocol/harbor.js';
  import { ProtocolError, isUnknownMethod } from '$lib/protocol/errors.js';
  import type { TopologyProjection } from '$lib/protocol/topology.js';
  import type { RunOverrides } from '$lib/protocol/runs.js';
  import { resolveConnection, hasScope, type RuntimeConnection } from '$lib/connection.js';
  import { openListPageDB } from '$lib/db/console_db.js';
  import { operatorIdOf } from '$lib/db/schema.js';
  import {
    loadSessionHistory,
    reduceHistoryTurns,
    type HistoryTurn
  } from '$lib/sessions/history.js';
  import {
    loadTurnPage,
    mergeTurnPages,
    reconcileTurnRow,
    TurnPageError,
    TURN_PAGE_DEFAULT_LIMIT,
    type TurnPage
  } from '$lib/sessions/turns.js';
  import { applyChunk, applyReasoningChunk, finalizeStream } from './chunk-stream.js';
  import type { ChatToolCall } from '$lib/chat/types.js';
  import {
    decodeChunk,
    decodeCost,
    decodeLifecycle,
    decodeBudget,
    decodePlannerDecision,
    decodeToolLifecycle,
    decodeIntervention,
    decodeInterventionClear,
    decodeAppAvailable,
    type ChunkEvent,
    type CostEvent,
    type LifecycleEvent,
    type ToolLifecycleEvent,
    type AppAvailableEvent
  } from './wire-events.js';
  import {
    appViewFromDiscovery,
    hydratedAgentMessage,
    turnRowMessages,
    type TurnRowMessages
  } from './turn-projection.js';
  import {
    PlaygroundSavedFilters,
    type PlaygroundViewSpec
  } from '$lib/db/saved_filters_playground.js';

  /* ---- props (test injection) ------------------------------------ */
  let {
    client: injectedClient
  }: { client?: ProtocolClient } = $props();

  /* ---- connection + client (CONVENTIONS.md §6) -------------------- */
  let connection = $state<RuntimeConnection | null>(null);
  let client = $state<ProtocolClient | null>(null);
  let canControl = $state(false);

  /* ---- the URL session-id discriminant --------------------------- */
  const sessionID = $derived(page.params.session_id ?? '');

  /* ---- page-level async state (the four-state contract) ----------- */
  let status = $state<PageStatus>('loading');
  let pageError = $state<ProtocolError | { code: string; message: string } | null>(null);
  // Phase 83w-F5 / D-164 — the friendly "topology not available on this
  // Runtime" info banner. Mirrors the live-runtime page's handling of
  // `unknown_method` on topology.snapshot. The initial load uses the
  // topology call as a connectivity probe; on a planner/RunLoop runtime
  // the chat surface is still fully functional, so the page degrades
  // to `ready` with messages flowing — the trace toggle becomes the
  // surface that surfaces the info banner instead.
  let pageInfo = $state<{ headline: string; detail: string } | null>(null);
  // A non-fatal note about the reopen hydration: set when the durable turn
  // page was partial (retention eviction), when an older page's cursor was
  // refused (typed expiry/gap — never a silent reset), or when hydration
  // failed for an UNEXPECTED reason (a real transport error — never a silent
  // swallow, CLAUDE.md §13). Null when hydration loaded cleanly (or the
  // runtime simply has no `sessions.turns.*` surface / no prior history).
  let historyNotice = $state<string | null>(null);
  // True when the runtime predates the `sessions.turns.*` surface
  // (`unknown_method` on the two-read open). The normal open NEVER silently
  // degrades to forensic event replay — the operator must explicitly invoke
  // it (CLAUDE.md §13, D-425: the legacy path is a user-invoked
  // degraded/forensic action, never the default).
  let turnProjectionUnavailable = $state(false);
  // True once the operator explicitly requested the degraded/forensic
  // `state.history` event-replay reopen.
  let legacyReopenRequested = $state(false);

  /* ---- durable turn-page paging (HA-64 / D-425) -------------------- */
  // The loaded newest-first turn pages (page[0] is the newest). Older pages
  // load through the opaque `next_older_cursor`, merged with stable ordering
  // under append (`mergeTurnPages` dedupes by turn_id — no skip/duplicate
  // while a new turn starts).
  let turnPages = $state<TurnPage[]>([]);
  let olderLoading = $state(false);
  let olderError = $state<string | null>(null);

  /* ---- chat stream ------------------------------------------------ */
  let messages = $state<ChatMessage[]>([]);
  let sending = $state(false);

  /* ---- header ----------------------------------------------------- */
  let activeAgent = $state('default agent');
  let tokenCount = $state(0);
  let costUSD = $state(0);
  let running = $state(false);
  let paused = $state(false);

  /* ---- Phase 108 KPI strip state ---------------------------------- */
  // All KPI numerics derive from REAL runtime events (no synthetic
  // placeholders — CLAUDE.md §13). Tokens + cost come from the
  // `llm.cost.recorded` event (the `tasks.get` cost rollup is 0 for
  // foreground dev turns); per-turn latency from `tasks.get` duration_ms;
  // the ceiling from `governance.budget_exceeded`. A metric with no
  // reading yet renders an em-dash, never a fake number.
  let tokenSamples = $state<number[]>([]);
  let turnLatencies = $state<number[]>([]);
  let ceilingUSD = $state<number | null>(null);
  let promptTokens = $state(0);
  let outputTokens = $state(0);
  // ISO timestamp of the session's first turn — drives the KPI Started
  // column. Set on the first send; null until then.
  let sessionStartedAt = $state<string | null>(null);
  // Total active-work time (ms) across all completed turns in this session
  // — the summed per-turn `duration_ms` (foreground + background), i.e. the
  // time the system was actually doing something (thinking + tool calls),
  // NOT wall-clock since the session opened. Drives the KPI Duration.
  let activeWorkMs = $state(0);
  // Epoch ms the in-flight turn started (0 when idle) — lets Duration tick
  // up live only while a turn is actually running.
  let activeTurnStartMs = $state(0);

  // D-171 — the connection's other conversations (sessions.list), for the
  // session switcher. One token, many sessions.
  let sessionList = $state<Array<{ session_id: string; last_activity_at?: string; title?: string }>>([]);
  // D-288 — inline rename of the ACTIVE session (sessions.set_title).
  let renamingActive = $state(false);
  let renameDraft = $state('');
  let renameBusy = $state(false);
  let renameError = $state<string | null>(null);
  // True once at least one `llm.cost.recorded` reading has landed — gates
  // the Cost tile so it shows "—" rather than a fabricated $0.0000.
  let hasCostReading = $state(false);
  // Per-task token/cost accumulator (108a-C) — summed from the task's
  // `llm.cost.recorded` events, attached to the agent bubble as per-turn
  // meta on completion. Not reactive (read once at terminal).
  const turnCost: Record<string, { tokens: number; cost: number }> = {};
  // 108a-C — per-task tool-call trace, collected from `planner.decision`
  // CallTool events during the turn and attached to the agent bubble on
  // completion. The runtime emits the tool NAME + decision kind via
  // planner.decision (there is no richer tool.* event), so args/timing are
  // not shown — only the honest tool name + status.
  const turnTools: Record<string, ChatToolCall[]> = {};
  // 108a-D composer telemetry: live tokens/sec (from content-chunk rate)
  // and the current context size (the last LLM call's prompt tokens).
  let tokensPerSec = $state(0);
  let lastPromptTokens = $state(0);
  let contextWindow = $state(0);
  let streamChars = 0;
  let streamStartMs = 0;

  /* ---- stream-liveness (composer telemetry "Session live") -------- */
  let eventsStreamLive = $state(false);

  /* ---- impersonation (admin only — D-107) ------------------------- */
  let impersonationTargets = $state<ImpersonationTarget[]>([]);
  let activeImpersonation = $state<ImpersonationTarget | null>(null);

  /* ---- right-rail: controls -------------------------------------- */
  let overridesPending = $state(false);
  let overridesResult = $state<{ ok: boolean; message: string } | null>(null);

  /* ---- right-rail: interventions + artifacts ---------------------- */
  let interventions = $state<PendingIntervention[]>([]);
  let recentArtifacts = $state<RecentArtifactEntry[]>([]);

  /* ---- trace toggle (Phase 74 topology.snapshot) ------------------ */
  let traceOn = $state(false);
  let traceNodes = $state<TraceNode[]>([]);
  let traceLoading = $state(false);
  let traceError = $state('');

  /* ---- pagination over the message stream ------------------------- */
  let pageIndex = $state(1);
  let pageSize = $state(50);

  /* ---- saved views (Console-DB-backed, D-061) --------------------- */
  let savedFilters = $state<PlaygroundSavedFilters | null>(null);
  let savedViews = $state<SavedView[]>([]);
  let savedSpecs = $state<Map<string, PlaygroundViewSpec>>(new Map());
  let activeSavedId = $state<string | null>(null);
  let saveName = $state('');


  /* ================================================================ */
  /* Derived                                                           */
  /* ================================================================ */

  // The active model + planner names. `modelName` is captured live from
  // the first `llm.cost.recorded` event (the real provider/model string);
  // `plannerName` is not exposed on the dev Protocol surface, so it stays
  // empty and the Header omits the pill rather than inventing a value.
  let modelName = $state('—');
  let plannerName = $state('');

  // The message page-window — real pagination over the stream.
  const pagedMessages = $derived<ChatMessage[]>(
    messages.slice((pageIndex - 1) * pageSize, pageIndex * pageSize)
  );

  /* ================================================================ */
  /* Error helper                                                      */
  /* ================================================================ */

  function toError(err: unknown): { code: string; message: string } {
    if (err instanceof ProtocolError) {
      return { code: err.code, message: err.message };
    }
    return {
      code: 'runtime_error',
      message: err instanceof Error ? err.message : 'unknown error'
    };
  }

  /* ================================================================ */
  /* The ChatProtocolClient adapter (D-091)                            */
  /* ================================================================ */

  // The page builds a ChatProtocolClient adapter over the Console
  // HarborClient and injects it into <ChatPanel>. The chat module
  // depends ONLY on this interface — it never touches HarborClient,
  // connection.ts, or fetch directly (CLAUDE.md §4.5 #11).
  // Round-7 F12 — Bytes go to the artifact store base64-encoded per the
  // wire shape (`ArtifactsPutRequest.Bytes` is `[]byte` on the Go side,
  // JSON-encoded as a base64 string). The browser's `FileReader.readAsDataURL`
  // yields a `data:<mime>;base64,<payload>` URL; we strip the prefix to
  // get the raw base64 the server expects.
  async function fileToBase64(file: File): Promise<string> {
    return new Promise((resolve, reject) => {
      const reader = new FileReader();
      reader.onerror = () => reject(reader.error ?? new Error('FileReader error'));
      reader.onload = () => {
        const result = reader.result;
        if (typeof result !== 'string') {
          reject(new Error('FileReader did not return a string'));
          return;
        }
        const comma = result.indexOf(',');
        resolve(comma >= 0 ? result.slice(comma + 1) : result);
      };
      reader.readAsDataURL(file);
    });
  }

  function buildChatClient(c: ProtocolClient): ChatProtocolClient {
    return {
      async sendMessage(text, artifactIDs, mode, dispositions) {
        // Round-6 F7 — the Playground V1 chat surface spawns a fresh
        // foreground task per operator turn (no run in flight); session-
        // scoped memory (D-149) carries the conversation across turns.
        //
        // Round-6 F10 — when a run is already in flight the operator
        // picks between two paths via the composer's mode picker:
        //   - 'steer' → inject the message into the running task via the
        //     SHIPPED `user_message` control verb (Phase 54). The
        //     runtime's run loop picks the message up on its next
        //     planner turn.
        //   - 'queue' → stash the message locally and dispatch via
        //     `start` once the current task reaches a terminal state.
        //     The lifecycle watcher below (subscribeEvents)
        //     auto-drains the queue when activeTaskID becomes null.
        //
        // Round-7 F11 / D-166 — multimodal artifact inputs. The
        // composer's chat-attach uploads each File via `artifacts.put`
        // and tracks the returned ids; sendMessage now plumbs them
        // through `control.start` (or stashes them on the queue path).
        // The runtime resolves each id to a `planner.InputArtifactView`
        // and renders per MIME on the first planner turn: image/*
        // inlines as `ImagePart.DataURL` (Path 1); everything else
        // stays as an `ArtifactStub` ref the LLM routes via the tool
        // catalog (operators register tools with `HandlesMIME` to
        // get the routing hint baked into the stub).
        //
        // user_message steering today carries only `{message: string}`
        // — mid-run artifact attachment is a separate seam (an
        // extension to the user_message payload). V1.1 limits
        // multimodal to start; mid-run inject stays text-only and we
        // surface a brief notice to the operator when they pick
        // 'steer' with attachments.
        if (mode === 'steer' && activeTaskID !== null) {
          if (artifactIDs.length > 0) {
            // No silent degradation — surface the gap and let the
            // operator decide whether to re-send as Queue. The chat
            // appears as a system bubble (the page-level error path
            // catches the throw and renders it).
            throw new Error(
              'steering attachment not supported: V1.1 inject is text-only; queue or wait for the run to finish.'
            );
          }
          await c.control.dispatch('user_message', activeTaskID, { message: text });
          return { taskID: activeTaskID };
        }
        if (mode === 'queue' && activeTaskID !== null) {
          // Stash for the lifecycle watcher to drain when the run
          // terminates. Multiple queued sends are FIFO.
          queuedSends = [...queuedSends, { text, artifactIDs, dispositions }];
          return { taskID: activeTaskID };
        }
        const resp = await c.control.start<{ task_id: string }>(text, {
          description: `Playground turn · ${activeAgent}`,
          inputArtifactIDs: artifactIDs,
          // Phase 84b (D-189) — per-attachment disposition hints from
          // the composer's selector; undefined defers to the agent's
          // multimodal.disposition policy / the runtime default.
          inputArtifactDispositions: dispositions
        });
        activeTaskID = resp.task_id;
        // Anchor the live Duration tick — Duration counts up only while a
        // turn is actually in flight (cleared on terminal).
        activeTurnStartMs = Date.now();
        return { taskID: resp.task_id };
      },
      async setOverrides(overrides) {
        // Typed as the named `RunOverrides` wire shape (NOT a loose
        // Record) so a phantom key — e.g. the removed `top_p` — is a
        // compile error rather than a request the runtime 400s wholesale
        // at its `DisallowUnknownFields()` decoder (D-223 blind-spot fix).
        const payload: RunOverrides = { session_id: sessionID };
        if (overrides.reasoningEffort !== undefined) {
          payload.reasoning_effort = overrides.reasoningEffort;
        }
        if (overrides.temperature !== undefined) {
          payload.temperature = overrides.temperature;
        }
        if (overrides.maxTokens !== undefined) {
          payload.max_tokens = overrides.maxTokens;
        }
        if (overrides.systemPromptOverride !== undefined) {
          payload.system_prompt_override = overrides.systemPromptOverride;
        }
        await c.runs.setOverrides(payload);
      },
      async uploadArtifact(file) {
        // Round-7 F12 — the prior implementation shipped a flat
        // `{filename, mime, size_bytes}` body and read `resp.id`. The
        // wire's `ArtifactsPutRequest` actually expects
        // `{scope, bytes, opts:{mime_type, filename}}` and returns
        // `{ref:{id, mime_type, ...}, protocol_version}`. The result:
        // the chat-attach flow always produced empty artifact ids
        // (`InputArtifactIDs: ['']` on the spawned task) and the
        // bytes never reached the store. Wire-shape correction here.
        const bytesB64 = await fileToBase64(file);
        const mime = file.type || 'application/octet-stream';
        const resp = await c.artifacts.put<{
          ref: { id: string; mime_type: string; size_bytes: number };
        }>({
          scope: {
            TenantID: connection!.identity.tenant,
            UserID: connection!.identity.user,
            SessionID: connection!.identity.session
          },
          bytes: bytesB64,
          opts: {
            mime_type: mime,
            filename: file.name
          }
        });
        if (!resp.ref || !resp.ref.id) {
          throw new Error('artifacts.put returned no ref id');
        }
        return {
          id: resp.ref.id,
          mime: resp.ref.mime_type || mime,
          filename: file.name,
          sizeBytes: resp.ref.size_bytes ?? file.size
        };
      },
      async resolveArtifact(id) {
        // `artifacts.get_ref` — the read-side presigned-URL resolver
        // (D-026 — renderers fetch from the presigned URL, never inline).
        // The wire field is `presigned_url`
        // (internal/protocol/types/artifacts.go::ArtifactsGetRefResponse); the
        // prior `resp.url` read resolved to `undefined` and silently broke
        // every chat-bubble artifact preview (§17.6 bug-twin of the heavy
        // MCP-App fetch this phase wires up).
        const resp = await c.artifacts.getRef<{ presigned_url: string }>({ id });
        return resp.presigned_url;
      },
      async cancelRun(hard) {
        await c.control.dispatch('cancel', sessionID, { hard });
      },
      async restartRun() {
        // Round-6 F7 — same shape correction as sendMessage. `start` is
        // not a steering verb; the typed `control.start` method ships
        // the correct `{identity:triple, task:{query,kind}}` shape.
        const resp = await c.control.start<{ task_id: string }>('', {
          description: `Playground restart · ${activeAgent}`
        });
        return { taskID: resp.task_id };
      },
      async approveIntervention(runID, pauseToken) {
        await c.control.dispatch('approve', runID, { token: pauseToken });
      },
      async rejectIntervention(runID, pauseToken) {
        await c.control.dispatch('reject', runID, { token: pauseToken });
      }
    };
  }

  let chatClient = $state<ChatProtocolClient | null>(null);

  /* ================================================================ */
  /* MCP Apps DisplayMode layout (fullscreen / pip)                    */
  /* ================================================================ */

  // The page-level layout state machine that honours an active MCP App's
  // DisplayMode (D-062): `fullscreen` (the app replaces the chat + composer
  // region, addressable via a tab strip) and `pip` (a resizable 50/50 split
  // with the right rail hidden by default). `inline` apps render inside the
  // chat scroll (109b) and never enter this model. The pure reducer +
  // projection live in `layout.ts`; this page is the only stateful holder.
  let layout = $state<LayoutModel>(INITIAL_LAYOUT);
  const region = $derived(computeRegion(layout));

  // The injected Protocol surface the hosted App panels drive app→host
  // requests through (D-173). Built once the connection resolves.
  let appHostClient = $state<MCPAppHostClient | null>(null);

  function dispatchLayout(action: LayoutAction): void {
    layout = reduceLayout(layout, action);
  }

  // The app (or the operator) requested a different display mode — switch the
  // page region at runtime WITHOUT reloading the session (D-062).
  function appRequestMode(app: OpenApp, mode: DisplayMode): void {
    const { mode: _drop, ...ref } = app;
    void _drop;
    dispatchLayout({ type: 'request-display-mode', app: ref, mode });
  }

  // The display modes the page can apply for an inline app — the full set,
  // because the page owns the page-level fullscreen / pip layout. Passed to
  // the chat module so an inline app's `onrequestdisplaymode` is granted and
  // routed back here (the inline-only chat-scroll default never grows).
  const APP_DISPLAY_MODES: McpUiDisplayMode[] = ['inline', 'fullscreen', 'pip'];

  // Derive a short tab/panel label from a `ui://` resource URI.
  function deriveAppTitle(resourceUri: string): string {
    const tail = resourceUri.replace(/^ui:\/\//, '').split('/').filter(Boolean).pop();
    return tail && tail !== '' ? tail : 'App';
  }

  // mcp.app_available — attach the discovered MCP App ref to the run's agent
  // bubble so the inline renderer mounts. Keyed by the discovery's run (=
  // the foreground turn's task id). A discovery with no matching agent bubble
  // (the bubble is created synchronously on send, before tool results) is a
  // no-op rather than a synthetic bubble.
  function applyAppAvailable(ev: AppAvailableEvent): void {
    // The projection lives in `turn-projection.ts` beside the replay one, so a
    // spec can pin the two producers against each other (a one-sided change
    // here would otherwise pass unnoticed).
    const { app: appView, serverID } = appViewFromDiscovery(ev);
    messages = messages.map((m) =>
      m.taskID === ev.taskID && m.role === 'agent' ? { ...m, app: appView, serverID } : m
    );
  }

  // An inline app (in the chat bubble) requested a larger display mode. Open
  // it as a page-level app so the fullscreen / pip layout takes over WITHOUT
  // reloading the session (D-062). An `inline` grant is a no-op — the app
  // already renders in the chat scroll.
  function onInlineAppDisplayModeRequest(
    req: DisplayModeRequest,
    app: MCPAppRefView,
    serverID: string
  ): void {
    if (req.granted === 'inline') return;
    const ref: AppRef = {
      id: appId(serverID, app.resourceUri),
      title: deriveAppTitle(app.resourceUri),
      serverID,
      resourceUri: app.resourceUri,
      rawHtmlTrusted: app.rawHtmlTrusted,
      // Carry the correlation key so the page-level (fullscreen / pip) render
      // delivers the same captured tool context the inline render does.
      toolCallId: app.toolCallId,
      // …and names the same originating tool in the host-context `toolInfo`.
      toolName: app.toolName
    };
    dispatchLayout({ type: 'request-display-mode', app: ref, mode: req.granted });
  }

  /* ================================================================ */
  /* Round-6 F10 — active-task lifecycle + queued-send drain           */
  /* ================================================================ */

  // The task id of the currently-running foreground run (null when no
  // run is in flight). `<ChatComposer>` reads this via the `running`
  // prop to decide whether to show the queue-vs-steer mode picker; the
  // sendMessage adapter consults it to route the message correctly.
  let activeTaskID = $state<string | null>(null);

  // HA-64 / D-425 (P1) — the task ids of durable turn rows the fold
  // rendered while still in flight (`pending` / `running` / `paused`).
  // This page did NOT start these tasks (activeTaskID stays null for
  // them), so without this admission `handleChunk` / `handleTerminal`
  // would reject every live chunk and the terminal event, and the
  // reopened bubble would freeze at the fold snapshot forever. The set
  // is populated ONLY from authoritative `sessions.turns.list` rows in
  // `foldTurnRows` and pruned on terminal convergence — never widened
  // to arbitrary session events. Plain (non-reactive) because it is read
  // only from event handlers, never rendered. The fold — and therefore
  // this membership — completes BEFORE the EventSource opens (`load`
  // awaits `hydratePastTurns` first), so a replayed terminal frame
  // (the snapshot-to-live resume cursor) always finds its task already
  // admitted here.
  let reopenedLiveTaskIDs = new Set<string>();

  // The durable row statuses that keep a fold-rendered turn eligible for
  // the live lane (the closed in-flight set).
  const LIVE_TURN_STATUSES = new Set(['pending', 'running', 'paused']);

  // HA-64 / D-425 (P1) — the snapshot-to-live handoff: the durable page's
  // `live_resume_seq` — the exclusive subscribe-from cursor — is bound as
  // the narrowly named `?resume_seq=` SSE query param when the fold
  // produced a non-zero cursor (captured from the `sessions.turns.list`
  // response in `hydratePastTurns`). `load` folds the durable rows and
  // establishes `reopenedLiveTaskIDs` BEFORE opening the EventSource, so a
  // terminal event that landed between the snapshot and the physical
  // subscription is replayed by the stream server (strictly-newer-than-
  // cursor, and only when no reconnect Last-Event-ID header is present)
  // and converges exactly like any other terminal frame — the frozen-bubble
  // race is closed without polling or a generic live-cursor API.

  // FIFO queue of "send when current run terminates" messages. The
  // lifecycle watcher below drains the queue with `start` calls as
  // soon as activeTaskID becomes null.
  let queuedSends = $state<
    Array<{ text: string; artifactIDs: string[]; dispositions?: Record<string, string> }>
  >([]);

  // Run phase derived from real stream + task state (no invented planner
  // state machine — CLAUDE.md §13 / decision #1). 'streaming' while
  // content deltas are flowing, 'active' while a task is in flight,
  // 'idle' otherwise.
  const isStreaming = $derived(messages.some((m) => m.streaming === true));
  const runPhase = $derived<'streaming' | 'active' | 'idle'>(
    isStreaming ? 'streaming' : activeTaskID !== null ? 'active' : 'idle'
  );

  // Best-effort EventSource subscription for task lifecycle. Filters
  // to the terminal task events scoped by this session's identity; the
  // bus auto-scopes to the bearer's (tenant, user, session) so the
  // page receives only its own session's events. The subscription is
  // optional — if the runtime returns 404/unknown_method (a build
  // without SSE wiring), the page degrades gracefully (the queue
  // simply does not auto-drain; the operator can still send by
  // pressing Send manually after the run completes).
  let taskEvents = $state<EventSource | null>(null);

  // KPI sample-buffer caps — the sparkline plots the last 60 token
  // observations; p50 latency is computed over the last 20 turns.
  const TOKEN_SAMPLE_CAP = 60;
  const LATENCY_SAMPLE_CAP = 20;

  // recordCost folds one `llm.cost.recorded` reading into the session
  // KPI totals (cumulative tokens + cost) and pushes the per-call token
  // total into the sparkline buffer. A ReAct turn fires this once per
  // LLM call, so a multi-step turn contributes multiple samples — all
  // real, none synthetic.
  function recordCost(ev: CostEvent): void {
    tokenCount += ev.totalTokens;
    promptTokens += ev.promptTokens;
    outputTokens += ev.outputTokens;
    costUSD += ev.usd;
    if (ev.model !== '') modelName = ev.model;
    if (ev.promptTokens > 0) lastPromptTokens = ev.promptTokens;
    if (ev.contextWindow > 0) contextWindow = ev.contextWindow;
    hasCostReading = true;
    const prev = turnCost[ev.taskID] ?? { tokens: 0, cost: 0 };
    turnCost[ev.taskID] = { tokens: prev.tokens + ev.totalTokens, cost: prev.cost + ev.usd };
    const next = [...tokenSamples, ev.totalTokens];
    tokenSamples = next.length > TOKEN_SAMPLE_CAP ? next.slice(-TOKEN_SAMPLE_CAP) : next;
  }

  // applyToolLifecycle reflects a tool.invoked/completed/failed event onto
  // the in-flight turn's tool-call rows. `planner.decision` adds the row
  // ('invoked') when the planner chooses the tool; the lifecycle events
  // resolve it to 'succeeded' / 'failed' (with a duration / error summary).
  // The first still-'invoked' row for the tool is the one that resolves;
  // a terminal event with no matching open row appends one (so a failure
  // is never silently dropped). This is what replaced the old
  // handleTerminal blanket "mark every tool succeeded" — a timed-out tool
  // now shows as failed.
  function applyToolLifecycle(ev: ToolLifecycleEvent): void {
    const status = ev.kind === 'completed' ? 'succeeded' : ev.kind === 'failed' ? 'failed' : 'invoked';
    const list = [...(turnTools[ev.taskID] ?? [])];
    let resolved = false;
    for (let i = 0; i < list.length && !resolved; i++) {
      if (list[i].tool === ev.tool && list[i].status === 'invoked') {
        if (ev.kind !== 'invoked') {
          list[i] = { ...list[i], status, summary: ev.summary !== '' ? ev.summary : list[i].summary };
        }
        resolved = true;
      }
    }
    if (!resolved) {
      list.push({ tool: ev.tool, status, summary: ev.kind === 'invoked' ? '' : ev.summary });
    }
    turnTools[ev.taskID] = list;
    messages = messages.map((m) =>
      m.taskID === ev.taskID && m.role === 'agent' ? { ...m, toolCalls: [...list] } : m
    );
  }

  // recordTurn pushes a completed turn's active duration (the `tasks.get`
  // duration_ms) into the p50 buffer AND adds it to the session's summed
  // active-work time (the KPI Duration).
  function recordTurn(durationMs: number): void {
    if (durationMs <= 0) return;
    activeWorkMs += durationMs;
    const next = [...turnLatencies, durationMs];
    turnLatencies = next.length > LATENCY_SAMPLE_CAP ? next.slice(-LATENCY_SAMPLE_CAP) : next;
  }

  // handleChunk streams one decoded `llm.completion.chunk` into the
  // pending agent bubble. Only the `content` channel grows the answer
  // body; `reasoning` deltas land in the accordion at completion, not
  // inline. Done flips the bubble's streaming flag off.
  //
  // HA-64 / D-425 (P1) — a chunk is admitted for the locally-started
  // task (activeTaskID) OR for a reopened in-flight turn the fold
  // rendered from `sessions.turns.list` (reopenedLiveTaskIDs); any other
  // task id is ignored, so the live lane never widens to arbitrary
  // session events. For a reopened turn only the `content` channel is
  // projected: its durable bubble already carries the row's DERIVED
  // reasoning summary, and appending raw thinking would corrupt it — the
  // sealed row's derived summary converges at terminal instead.
  function handleChunk(ev: ChunkEvent): void {
    if (ev.taskID !== activeTaskID && !reopenedLiveTaskIDs.has(ev.taskID)) return;
    if (ev.delta !== '') {
      // The content channel grows the answer body; the reasoning channel
      // grows the live "Reasoning" disclosure (108a — runtime reasoning
      // emit fixed in the corrections layer). Neither pollutes the other.
      if (ev.kind === 'reasoning') {
        if (ev.taskID === activeTaskID) {
          messages = applyReasoningChunk(messages, ev.taskID, ev.delta);
        }
      } else {
        messages = applyChunk(messages, ev.taskID, ev.delta);
        // 108a-D — live tokens/sec from the content-chunk rate.
        if (streamStartMs === 0) streamStartMs = Date.now();
        streamChars += ev.delta.length;
        const elapsedS = (Date.now() - streamStartMs) / 1000;
        if (elapsedS > 0.2) tokensPerSec = streamChars / 4 / elapsedS;
      }
    }
    if (ev.done) {
      messages = finalizeStream(messages, ev.taskID);
    }
  }

  // Converges one task's agent bubble to a post-terminal message, or
  // inserts the message right after the turn's user bubble when the fold
  // rendered no agent bubble for it (a reopened turn that was running
  // with nothing renderable at reopen). `fallback === null` inserts
  // nothing. The insert only runs when no agent message for the task
  // matched, so it can never duplicate the bubble; it keeps timeline
  // order by sitting directly under the turn's user bubble.
  function convergeOrInsertAgent(
    msgs: ChatMessage[],
    taskID: string,
    update: (m: ChatMessage) => ChatMessage,
    fallback: ChatMessage | null
  ): ChatMessage[] {
    let matched = false;
    const mapped = msgs.map((m) => {
      if (m.taskID !== taskID || m.role !== 'agent') return m;
      matched = true;
      return update(m);
    });
    if (matched || fallback === null) return mapped;
    const idx = mapped.findIndex((m) => m.taskID === taskID && m.role === 'user');
    if (idx === -1) return [...mapped, fallback];
    return [...mapped.slice(0, idx + 1), fallback, ...mapped.slice(idx + 1)];
  }

  // handleTerminal reconciles a completed/failed/cancelled turn. On
  // completion it fetches the authoritative sealed turn via ONE
  // `sessions.turns.get` (HA-64 / D-425 — the bounded terminal reconciliation
  // read; it does NOT refetch the raw transcript) and records the turn
  // latency; on failure/cancellation it converts the bubble to a system
  // error. The reconcile projects the sealed durable row (answer union,
  // derived reasoning summary, usage availability, bounded activity, App
  // refs) so the completed bubble renders from the SAME consumer-safe read
  // model the reopen path renders — identical skeletons before/after restart.
  //
  // HA-64 / D-425 (P1) — the reconcile ALSO admits a reopened in-flight
  // turn (a task the fold rendered from `sessions.turns.list` while it was
  // still running/paused, tracked in `reopenedLiveTaskIDs`): its terminal
  // event converges its durable bubble to the sealed row exactly like the
  // locally-started path, with exactly one `sessions.turns.get`. The task
  // is pruned from the eligible set BEFORE the read, so a redelivered
  // terminal frame is a no-op rather than a second reconcile.
  async function handleTerminal(ev: LifecycleEvent): Promise<void> {
    // Drop any pending intervention parked on this run — a terminal
    // task can never still be awaiting an operator decision.
    interventions = interventions.filter((i) => i.runID !== ev.taskID);
    const taskID = ev.taskID;
    const isActive = taskID === activeTaskID;
    const isReopened = reopenedLiveTaskIDs.has(taskID);
    if (!isActive && !isReopened) return;
    if (isReopened) reopenedLiveTaskIDs.delete(taskID);
    if (ev.kind === 'completed' && client !== null) {
      try {
        // ONE sessions.turns.get on the consumer lane. The sealed row is the
        // authoritative terminal snapshot — never a tasks.get / state.history /
        // events.list transcript join.
        const row = await reconcileTurnRow(client, sessionID, taskID);
        const rendered = turnRowMessages(row);
        const durationMs = rendered.agent?.meta?.elapsedMs ?? 0;
        recordTurn(durationMs);
        const tc = turnCost[taskID];
        const meta = {
          elapsedMs: durationMs > 0 ? durationMs : undefined,
          tokens: rendered.usage.tokens ?? tc?.tokens,
          costUSD: rendered.usage.costUSD ?? tc?.cost
        };
        const sealed: ChatMessage | null =
          rendered.agent !== null
            ? { ...rendered.agent, meta, pending: false, streaming: false }
            : null;
        messages = convergeOrInsertAgent(
          messages,
          taskID,
          (m) => ({
            ...m,
            text: rendered.agent?.text ?? '(no answer recorded)',
            // The durable row carries only DERIVED reasoning (structurally
            // no raw thinking) — the live bubble's streamed reasoning is
            // preserved unless the sealed row has a summary to show.
            reasoningText: rendered.agent?.reasoningText ?? m.reasoningText,
            reasoningSteps: m.reasoningSteps,
            toolCalls: rendered.agent?.toolCalls ?? m.toolCalls,
            artifacts: rendered.agent?.artifacts ?? m.artifacts,
            app: rendered.agent?.app ?? m.app,
            serverID: rendered.agent?.serverID ?? m.serverID,
            meta,
            pending: false,
            streaming: false
          }),
          sealed
        );
      } catch {
        messages = messages.map((m) =>
          m.taskID === taskID && m.role === 'agent'
            ? { ...m, text: '(could not read answer)', pending: false, streaming: false }
            : m
        );
      }
    } else if (ev.kind !== 'completed') {
      const errorText = `Task ${ev.kind} — see Tasks page for details.`;
      messages = convergeOrInsertAgent(
        messages,
        taskID,
        (m) => ({ ...m, text: errorText, role: 'system', pending: false, streaming: false }),
        {
          id: `m-${Date.now()}-sys`,
          role: 'system',
          text: errorText,
          taskID,
          at: new Date().toISOString(),
          pending: false
        }
      );
    }
    if (isActive) {
      running = false;
      paused = false;
      activeTaskID = null;
      // Stop the live Duration tick; the turn's real duration_ms was added to
      // activeWorkMs by recordTurn above.
      activeTurnStartMs = 0;
      void drainQueue();
    } else if (reopenedLiveTaskIDs.size === 0 && activeTaskID === null) {
      // The last reopened in-flight turn converged — the session has no
      // live work left, so the pause indicator the fold set no longer holds.
      paused = false;
    }
    // 108a-F — a completed turn may have produced artifacts (e.g. tool
    // results); refresh the Recent Artifacts card.
    void refreshArtifacts();
  }

  // Best-effort EventSource subscription. The bus auto-scopes to the
  // bearer's (tenant, user, session), so the page receives only its own
  // session's events. Every frame is the flat `wireEvent` projection
  // (`{type, payload:{...PascalCase}}`); the `wire-events.ts` decoders
  // own that shape (the prior cut read top-level snake_case and silently
  // dropped every chunk). The subscription is optional — a runtime
  // without the SSE surface leaves the stream Off and the page still
  // works (the operator sends manually; answers arrive via tasks.get).
  //
  // HA-64 / D-425 (P1) — `resumeSeq` is the durable fold's
  // `live_resume_seq` (from `hydratePastTurns`), seeded as the stream's
  // narrowly named initial-resume query param so a terminal event that
  // landed between the snapshot and this physical subscription is
  // replayed by the server and converges the rendered bubble instead of
  // freezing it. The page opens the stream ONLY after the fold completes
  // (`load` awaits `hydratePastTurns` first); a re-open (retry) closes
  // any previous EventSource first so no second stream leaks.
  function subscribeEvents(c: ProtocolClient, resumeSeq?: number): void {
    try {
      // A re-run of load() (the PageState retry) must not leak a second
      // EventSource alongside the first.
      if (taskEvents !== null) {
        taskEvents.close();
        taskEvents = null;
      }
      const url = c.events.subscribeURL({
        eventTypes: [
          'task.completed',
          'task.failed',
          'task.cancelled',
          'llm.completion.chunk',
          'llm.cost.recorded',
          'governance.budget_exceeded',
          'planner.decision',
          'tool.invoked',
          'tool.completed',
          'tool.failed',
          'mcp.app_available',
          'tool.approval_requested',
          'tool.auth_required',
          'pause.requested',
          'pause.resumed',
          'tool.approved',
          'tool.rejected',
          'tool.auth_completed',
          'session.title_changed'
        ],
        resumeSeq
      });
      const es = new EventSource(url);
      es.onopen = () => {
        eventsStreamLive = true;
      };
      es.addEventListener('llm.completion.chunk', (msg: MessageEvent) => {
        eventsStreamLive = true;
        const ev = decodeChunk((msg as MessageEvent<string>).data);
        if (ev !== null) handleChunk(ev);
      });
      es.addEventListener('llm.cost.recorded', (msg: MessageEvent) => {
        const ev = decodeCost((msg as MessageEvent<string>).data);
        if (ev !== null) recordCost(ev);
      });
      es.addEventListener('governance.budget_exceeded', (msg: MessageEvent) => {
        const ev = decodeBudget((msg as MessageEvent<string>).data);
        if (ev !== null) ceilingUSD = ev.ceilingUSD;
      });
      es.addEventListener('planner.decision', (msg: MessageEvent) => {
        const ev = decodePlannerDecision((msg as MessageEvent<string>).data);
        if (ev === null || ev.decisionKind !== 'CallTool' || ev.tool === '') return;
        // Collect the tool call for the in-flight turn; the live bubble
        // updates immediately so tool use is visible as it happens.
        const list = turnTools[ev.taskID] ?? [];
        list.push({ tool: ev.tool, status: 'invoked', summary: '' });
        turnTools[ev.taskID] = list;
        messages = messages.map((m) =>
          m.taskID === ev.taskID && m.role === 'agent'
            ? { ...m, toolCalls: [...list] }
            : m
        );
      });
      // Tool lifecycle — the runtime emits tool.invoked/completed/failed
      // (internal/tools/events.go). These carry the REAL terminal state, so
      // a timed-out / failed tool shows as failed instead of masquerading
      // as succeeded. planner.decision adds the row when the planner chooses
      // the tool; these update its status as the call actually resolves.
      const onToolLifecycle = (msg: MessageEvent): void => {
        const ev = decodeToolLifecycle((msg as MessageEvent<string>).data);
        if (ev !== null) applyToolLifecycle(ev);
      };
      es.addEventListener('tool.invoked', onToolLifecycle);
      es.addEventListener('tool.completed', onToolLifecycle);
      es.addEventListener('tool.failed', onToolLifecycle);

      // MCP App discovery — an MCP tool result on this turn declared a
      // `ui://` interactive app. Attach the ref to the run's agent bubble so
      // the inline renderer mounts (109b) and the page-level layout (109c)
      // activates when the app requests fullscreen / pip.
      es.addEventListener('mcp.app_available', (msg: MessageEvent): void => {
        const ev = decodeAppAvailable((msg as MessageEvent<string>).data);
        if (ev !== null) applyAppAvailable(ev);
      });

      const onTerminal = (msg: MessageEvent): void => {
        const ev = decodeLifecycle((msg as MessageEvent<string>).data);
        if (ev !== null) void handleTerminal(ev);
      };
      es.addEventListener('task.completed', onTerminal);
      es.addEventListener('task.failed', onTerminal);
      es.addEventListener('task.cancelled', onTerminal);

      // Pending interventions — the unified-pause family parks a run
      // awaiting an operator decision. A request event adds the row
      // (deduped by pause token, the unified correlation key); a
      // resume/terminal event clears it.
      const onInterventionRequest = (msg: MessageEvent): void => {
        const ev = decodeIntervention((msg as MessageEvent<string>).data);
        if (ev === null) return;
        const rest = interventions.filter((i) => i.pauseToken !== ev.pauseToken);
        interventions = [...rest, ev];
      };
      es.addEventListener('tool.approval_requested', onInterventionRequest);
      es.addEventListener('tool.auth_required', onInterventionRequest);
      es.addEventListener('pause.requested', onInterventionRequest);
      const onInterventionClear = (msg: MessageEvent): void => {
        const resolved = decodeInterventionClear((msg as MessageEvent<string>).data);
        if (resolved === null) return;
        interventions = interventions.filter((i) => i.pauseToken !== resolved.pauseToken);
      };
      es.addEventListener('pause.resumed', onInterventionClear);
      es.addEventListener('tool.approved', onInterventionClear);
      es.addEventListener('tool.rejected', onInterventionClear);
      es.addEventListener('tool.auth_completed', onInterventionClear);

      // D-288 — a rename of the ACTIVE session refreshes the switcher's
      // title labels. The SSE subscription is scoped to this page's own
      // (tenant, user, session) triple, so only the active session's
      // title_changed events arrive here — a sibling session renamed
      // elsewhere does NOT reach this page live (its label refreshes on
      // the next sessions.list read, e.g. a page re-mount or switch).
      // The event is content-free by design (identity scope + session id
      // + source only — never the title string), so the handler simply
      // refetches the projection rather than decoding a payload.
      es.addEventListener('session.title_changed', () => {
        void refreshSessionList();
      });
      es.onerror = () => {
        // EventSource auto-reconnects on transient drops; only nullify
        // on a permanent close to avoid resubscribe storms.
        if (es.readyState === EventSource.CLOSED) {
          eventsStreamLive = false;
          taskEvents = null;
        }
      };
      taskEvents = es;
    } catch {
      eventsStreamLive = false;
      taskEvents = null;
    }
  }

  async function drainQueue(): Promise<void> {
    if (chatClient === null || queuedSends.length === 0) {
      return;
    }
    const next = queuedSends[0];
    queuedSends = queuedSends.slice(1);
    try {
      // The drained send replays through sendMessage so the same
      // start-vs-steer routing applies (a queued message might land
      // while a NEW run is already in flight — back to 'queue' it
      // goes). The async push lands in the messages timeline via the
      // page's existing sendMessage handler.
      await sendMessage(next.text, next.artifactIDs, undefined, next.dispositions);
    } catch {
      // Errors surface through the page's existing sendMessage error
      // path; no retry here to avoid burying the operator's intent.
    }
  }

  /* ================================================================ */
  /* Session reload — the TWO-READ chat open (HA-64 / D-425)           */
  /* ================================================================ */

  // hydratePastTurns reloads the conversation's prior turns when opening an
  // existing session. The NORMAL open is exactly two reads (Phase 245 +
  // Phase 246 / D-425):
  //
  //   1. ONE lifecycle-only session read — `sessions.inspect` with
  //      `projection: 'lifecycle'` (HA-63 / D-424). The lifecycle row drives
  //      the header's paused flag, the KPI Started column, and the honest
  //      brand-new-session detection (`not_found` → empty start). It asks for
  //      NO counter enrichment — a lifecycle row's zeros mean "not computed",
  //      never "measured as zero".
  //   2. ONE durable tail page — `sessions.turns.list` (HA-64). The Runtime's
  //      consumer-safe conversation projection: query, answer/reference,
  //      attachments metadata, lifecycle, derived reasoning summaries, agent
  //      binding, per-measure usage availability, MCP App references with
  //      availability, pause state, and the bounded inline activity window.
  //      No `state.history`, no `tasks.list`, no `tasks.get`, no `events.list`.
  //
  // Older pages load through the page's opaque `next_older_cursor`
  // (`loadOlderTurns` below) — a snapshot/keyset cursor anchored on an
  // immutable task/turn tie-breaker, so a new turn starting while the
  // operator pages older history produces neither duplicates nor omissions.
  //
  // A runtime that predates the `sessions.turns.*` surface answers
  // `unknown_method`: the page sets `turnProjectionUnavailable` and OFFERS
  // the degraded/forensic legacy reopen (`hydratePastTurnsLegacy`, the
  // `state.history` event-replay path) — the operator must explicitly invoke
  // it. It is never the default open path (D-425).
  //
  // Returns the durable fold's `live_resume_seq` when it is non-zero (the
  // exclusive subscribe-from cursor the page binds to the EventSource's
  // initial resume query — the snapshot-to-live handoff), or `undefined`
  // when the fold produced no cursor (brand-new session, `unknown_method`,
  // `not_found`, a transport failure, or a page whose cursor is 0).
  async function hydratePastTurns(): Promise<number | undefined> {
    if (client === null || sessionID === '') return undefined;
    const c = client;
    historyNotice = null;
    olderError = null;
    turnProjectionUnavailable = false;
    turnPages = [];
    // HA-64 / D-425 (P1) — a re-hydration (retry) rebuilds the eligible
    // in-flight set from the fresh fold; stale admissions from a previous
    // hydration must not survive.
    reopenedLiveTaskIDs.clear();

    // ---- read 1 of 2: the lifecycle-only session snapshot (HA-63) ----
    try {
      const lifecycle = await new SessionsProtocol(c).inspect({
        session_id: sessionID,
        projection: 'lifecycle'
      });
      const row = lifecycle.row;
      if (row?.started_at && sessionStartedAt === null) {
        sessionStartedAt = row.started_at;
      }
      if (row?.status === 'paused') paused = true;
    } catch (err) {
      // `not_found` on inspect = the session has no catalog row yet (brand-new
      // session id, materialised create-on-first-use) — honest empty start.
      if (err instanceof ProtocolError && err.code === 'not_found') {
        return undefined;
      }
      if (isUnknownMethod(err)) {
        // Runtime predates the lifecycle projection — the turns read below
        // still decides whether the projection open is possible.
      } else {
        // A real lifecycle-read failure is non-fatal (the turn page is the
        // content source); the page proceeds to the turns read.
        console.warn('hydratePastTurns: lifecycle inspect failed:', String(err));
      }
    }

    // ---- read 2 of 2: ONE durable turn tail page (HA-64) ----
    try {
      const page = await loadTurnPage(c, { sessionID, limit: TURN_PAGE_DEFAULT_LIMIT });
      turnPages = [page];
      if (page.pageCompleteness === 'partial') {
        historyNotice =
          page.partialReason === 'retention_eviction'
            ? 'Older messages were trimmed by retention — showing the most recent history.'
            : 'The turn projection is partial — some older messages may be missing.';
      }
      // The resume cursor rides even an empty newest page: a non-zero
      // `live_resume_seq` means events exist beyond the fold (all-gated or
      // pre-turn activity), and seeding the stream cursor keeps the
      // snapshot-to-live handoff gap-free.
      const resumeSeq = page.liveResumeSeq > 0 ? page.liveResumeSeq : undefined;
      if (page.turns.length === 0) return resumeSeq;

      const rendered = page.turns.map((r) => turnRowMessages(r));
      foldTurnRows(rendered, page.turns);
      return resumeSeq;
    } catch (err) {
      if (err instanceof TurnPageError && err.kind === 'unknown_method') {
        // The Runtime predates the `sessions.turns.*` surface. The normal
        // open does NOT silently degrade to forensic replay — offer it.
        turnProjectionUnavailable = true;
        historyNotice =
          'This Runtime does not expose the conversation turn projection. You can load earlier messages with the degraded forensic event-replay path.';
        return undefined;
      }
      if (err instanceof TurnPageError && err.kind === 'not_found') {
        // Foreign/erased session — non-oracular empty start.
        return undefined;
      }
      const detail =
        err instanceof ProtocolError ? `${err.code}: ${err.message}` : String(err);
      console.warn('hydratePastTurns: turn projection read failed:', detail);
      historyNotice = `Could not load the conversation projection (${detail}). The conversation continues from here.`;
      return undefined;
    }
  }

  // foldTurnRows projects a page's durable rows into chat messages, dedupes
  // against the live stream, folds the honest per-component state into the
  // header/KPI accumulators, and prepends the rendered bubbles. The rows are
  // newest-first (the projection contract); the chat stream renders
  // oldest-at-top (newest at the bottom, matching the live view), so the
  // page's rows are iterated from the oldest to the newest before prepending.
  // The KPI fold is availability-gated: tokens/cost/latency are folded ONLY
  // when the wire reports them exact/estimated — an unavailable measure never
  // becomes a fabricated zero (D-425, CLAUDE.md §13).
  function foldTurnRows(renderedRows: TurnRowMessages[], rows: Array<{ task_id?: string; turn_id: string }>): void {
    const present = new Set(messages.map((m) => m.taskID).filter(Boolean));
    const hydrated: ChatMessage[] = [];
    let activityOverflow = 0;
    let attachmentsUnavailable = 0;
    let reasoningPartial = 0;
    for (let i = renderedRows.length - 1; i >= 0; i--) {
      const rendered = renderedRows[i];
      const row = rows[i];
      const taskID = row.task_id || row.turn_id;
      if (present.has(taskID)) {
        // HA-64 / D-425 (P1) — a page retry re-runs hydration with the
        // previous fold's bubbles STILL rendered. The row already has a
        // rendered message, so message/KPI insertion is skipped (no duplicate
        // bubble, no double-folded KPI) — but the live-lane admission must be
        // REBUILT: `reopenedLiveTaskIDs` was cleared at the top of hydration,
        // and without re-adding the freshly-read running/pending/paused row
        // the retried page loses the live admission and later chunks/terminal
        // events freeze. ONLY the closed live status set is re-admitted — a
        // terminal row never regains membership.
        if (LIVE_TURN_STATUSES.has(rendered.status)) {
          reopenedLiveTaskIDs.add(taskID);
        }
        continue;
      }
      const m = rendered;
      // HA-64 / D-425 (P1) — admit the fold-rendered in-flight rows to the
      // live lane: this page did not start them (activeTaskID stays null),
      // so without this admission handleChunk/handleTerminal would reject
      // every live chunk + the terminal event and the reopened bubble would
      // freeze at the fold snapshot forever. ONLY rows the fold actually
      // rendered count — a task already in the stream (`present`) was
      // skipped above and is never admitted.
      if (LIVE_TURN_STATUSES.has(rendered.status)) {
        reopenedLiveTaskIDs.add(taskID);
      }
      // Usage availability-gated KPI fold.
      const tokens = m.usage.tokens;
      const cost = m.usage.costUSD;
      if (tokens !== undefined || cost !== undefined) {
        hasCostReading = true;
        const prev = turnCost[taskID] ?? { tokens: 0, cost: 0 };
        turnCost[taskID] = {
          tokens: prev.tokens + (tokens ?? 0),
          cost: prev.cost + (cost ?? 0)
        };
      }
      if (tokens !== undefined) tokenCount += tokens;
      if (m.usage.promptTokens !== undefined) promptTokens += m.usage.promptTokens;
      if (m.usage.outputTokens !== undefined) outputTokens += m.usage.outputTokens;
      if (cost !== undefined) costUSD += cost;
      if (m.usage.model !== undefined && m.usage.model !== '') modelName = m.usage.model;
      if (m.usage.latencyMs !== undefined && m.usage.latencyMs > 0) recordTurn(m.usage.latencyMs);
      if (m.activityOverflow.more) activityOverflow += m.activityOverflow.dropped;
      attachmentsUnavailable += m.attachmentsUnavailable;
      if (m.reasoningPartial) reasoningPartial += m.reasoningDropped;
      if (m.paused && !paused) paused = true;

      if (m.user !== null) hydrated.push(m.user);
      if (m.agent !== null) hydrated.push(m.agent);
    }
    if (hydrated.length > 0) {
      // Prepend — the reloaded turns predate any live message sent after
      // reload, so history sits above the live tail.
      messages = [...hydrated, ...messages];
      if (sessionStartedAt === null && hydrated[0]?.at) sessionStartedAt = hydrated[0].at;
    }
    // Honest notices for bounded windows / unavailable components.
    const notes: string[] = [];
    if (activityOverflow > 0) {
      notes.push(`Some turns had tool activity beyond the inline window (${activityOverflow} older row(s) not shown).`);
    }
    if (attachmentsUnavailable > 0) {
      notes.push(`${attachmentsUnavailable} attachment(s) are unavailable.`);
    }
    if (reasoningPartial > 0) {
      notes.push(`${reasoningPartial} older reasoning step(s) not retained in some turns.`);
    }
    if (notes.length > 0) {
      historyNotice = historyNotice !== null ? `${historyNotice} ${notes.join(' ')}` : notes.join(' ');
    }
  }

  // loadOlderTurns pages one older page through the opaque
  // `next_older_cursor` and prepends it. A refused cursor (expired snapshot,
  // retention-evicted, foreign, forged) surfaces as a typed, honest notice —
  // never a silent reset to page one, never a fabricated empty page.
  async function loadOlderTurns(): Promise<void> {
    if (client === null || olderLoading) return;
    const last = turnPages[turnPages.length - 1];
    if (last === undefined || !last.hasMore || last.nextOlderCursor === undefined) return;
    olderLoading = true;
    olderError = null;
    try {
      const page = await loadTurnPage(client, {
        sessionID,
        olderCursor: last.nextOlderCursor,
        limit: TURN_PAGE_DEFAULT_LIMIT
      });
      const merged = mergeTurnPages([...turnPages, page]);
      turnPages = merged.pages;
      if (page.turns.length > 0) {
        foldTurnRows(page.turns.map((r) => turnRowMessages(r)), page.turns);
      }
    } catch (err) {
      if (err instanceof TurnPageError) {
        olderError =
          err.kind === 'invalid_cursor'
            ? 'The older-page cursor was refused (expired or stale snapshot). Reload the conversation to re-anchor.'
            : err.kind === 'not_found'
              ? 'The conversation was not found — older pages are unavailable.'
              : err.kind === 'unknown_method'
                ? 'This Runtime does not expose the turn projection.'
                : `Older turns could not be loaded (${err.message}).`;
      } else {
        olderError = `Older turns could not be loaded (${String(err)}).`;
      }
    } finally {
      olderLoading = false;
    }
  }

  // hydratePastTurnsLegacy is the EXPLICIT, user-invoked degraded/forensic
  // fallback (D-425): the pre-HA-64 reopen that reconstructs turns CLIENT-SIDE
  // from the `state.history` windowed event-replay surface (D-254), folding
  // the user query text in from a single `tasks.list` catalog lookup. It is
  // ONLY reachable via the "reopen via forensic event replay" control shown
  // when the turn projection is unavailable — never the default open path.
  async function hydratePastTurnsLegacy(): Promise<void> {
    if (client === null || sessionID === '') return;
    const c = client;
    historyNotice = null;
    try {
      // 1. Window the durable event stream tail-first, scrolling up by
      //    next_cursor (the `state.history` consumer), and reduce the
      //    loaded events into per-run turns client-side.
      const loaded = await loadSessionHistory(c, sessionID, { pageLimit: 50 });
      // Honest retention signal: when the durable substrate (or a wrapped
      // best-effort ring) dropped events older than the loaded window, say
      // so rather than presenting a trimmed history as complete (§13).
      if (loaded.truncated) {
        historyNotice =
          'Older messages were trimmed by retention — showing the most recent history.';
      }
      const turns: HistoryTurn[] = reduceHistoryTurns(loaded.events);
      if (turns.length === 0) return;

      // 2. One catalog lookup for the user query text + per-turn timing
      //    (the query is not in the event payloads). Keyed by run id.
      const catalog = new Map<string, { query: string; at: string; durationMs: number }>();
      try {
        const resp = await c.tasks.list<{
          rows?: Array<{
            id: string;
            query?: string;
            started_at?: string;
            duration_ms?: number;
          }>;
        }>({ filter: {}, page_size: 200 });
        for (const r of resp.rows ?? []) {
          catalog.set(r.id, {
            query: r.query ?? '',
            at: r.started_at ?? '',
            durationMs: r.duration_ms ?? 0
          });
        }
      } catch {
        /* query text is best-effort; agent answers still hydrate */
      }

      // Dedup against turns already in the stream so a live turn sent
      // before hydration finished is neither duplicated nor discarded.
      const present = new Set(messages.map((m) => m.taskID).filter(Boolean));
      const fresh = turns.filter((t) => !present.has(t.runID));
      if (fresh.length === 0) return;

      const hydrated: ChatMessage[] = [];
      for (const turn of fresh) {
        const meta = catalog.get(turn.runID);
        // Prefer the tasks.list duration_ms (the primary source, as today);
        // fall back to the reducer's task-lifecycle-derived durationMs.
        const durationMs = meta?.durationMs || turn.durationMs || 0;
        const at = meta?.at || turn.at || new Date().toISOString();
        const query = meta?.query ?? '';

        // Fold the reopened turn into the session KPI accumulators the header
        // + model chip read — so leave-and-return renders IDENTICAL to the live
        // view (the acceptance centerpiece), not just the message bodies.
        recordTurn(durationMs);
        tokenCount += turn.tokens;
        promptTokens += turn.promptTokens;
        outputTokens += turn.outputTokens;
        costUSD += turn.costUSD;
        if (turn.tokens > 0 || turn.costUSD > 0) {
          // A real reading exists — flip the KPI strip out of its "no turns
          // yet" em-dash state so the reopened stats render (the operator's
          // regression: reopen showed no tokens/cost).
          hasCostReading = true;
          const prev = turnCost[turn.runID] ?? { tokens: 0, cost: 0 };
          turnCost[turn.runID] = { tokens: prev.tokens + turn.tokens, cost: prev.cost + turn.costUSD };
        }
        if (turn.model !== '') modelName = turn.model;
        const toolCalls: ChatToolCall[] =
          turn.toolCalls.length > 0
            ? turn.toolCalls.map((tc) => ({ tool: tc.tool, status: tc.status, summary: tc.summary, runID: turn.runID }))
            : [];
        if (toolCalls.length > 0) turnTools[turn.runID] = [...toolCalls];

        if (query) {
          hydrated.push({ id: `h-${turn.runID}-u`, role: 'user', text: query, taskID: turn.runID, at });
        }
        // The field mapping (and its render gate) lives in `turn-projection.ts`
        // so a spec can hold it — inline here it was the one part of the replay
        // no test could reach.
        const agent = hydratedAgentMessage(turn, { at, durationMs, toolCalls });
        if (agent !== null) hydrated.push(agent);
      }
      if (hydrated.length === 0) return;
      // Prepend — the reloaded turns predate any live message sent after
      // reload, so history sits above the live tail.
      messages = [...hydrated, ...messages];
      if (sessionStartedAt === null && hydrated[0]?.at) sessionStartedAt = hydrated[0].at;
    } catch (err) {
      // Distinguish EXPECTED-empty from a real transport failure (§13 — no
      // silent swallow). A runtime that predates the surface
      // (`unknown_method`) or a session with no retained history
      // (`not_found`) legitimately starts empty. Any other error (e.g. the
      // bus has no windowed-read capability → `runtime_error`, or a network
      // fault) is surfaced as a non-fatal notice rather than masked behind a
      // blank conversation.
      if (isUnknownMethod(err) || (err instanceof ProtocolError && err.code === 'not_found')) {
        return;
      }
      const detail = err instanceof ProtocolError ? `${err.code}: ${err.message}` : String(err);
      console.warn('hydratePastTurnsLegacy: could not load earlier messages:', detail);
      historyNotice = `Could not load earlier messages (${detail}). The conversation continues from here.`;
    }
  }

  // refreshArtifacts loads this session's recent artifacts (108a-F). The
  // per-session client scopes artifacts.list to the conversation; tool
  // results land here (e.g. a `tool-result-*.json`).
  async function refreshArtifacts(): Promise<void> {
    if (client === null) return;
    try {
      const resp = await client.artifacts.list<{
        rows?: Array<{
          ref?: { id?: string; filename?: string; mime_type?: string; size_bytes?: number };
          created_at?: string;
        }>;
      }>({});
      recentArtifacts = (resp.rows ?? [])
        .map((r) => ({
          id: r.ref?.id ?? '',
          filename: r.ref?.filename ?? '(unnamed)',
          mime: r.ref?.mime_type ?? 'application/octet-stream',
          sizeBytes: r.ref?.size_bytes,
          createdAt: r.created_at
        }))
        .filter((a) => a.id !== '')
        .sort((a, b) => (b.createdAt ?? '').localeCompare(a.createdAt ?? ''));
    } catch {
      recentArtifacts = [];
    }
  }

  // 108a-F — the runtime's tool catalog, for the Controls "Tools" row.
  let toolList = $state<string[]>([]);
  async function refreshTools(): Promise<void> {
    if (client === null) return;
    try {
      const resp = await client.tools.list<{
        tools?: Array<{ id?: string; name?: string }>;
        rows?: Array<{ id?: string; name?: string }>;
      }>();
      const rows = resp.tools ?? resp.rows ?? [];
      toolList = rows.map((t) => t.id ?? t.name ?? '').filter((s) => s !== '');
    } catch {
      toolList = [];
    }
  }

  // refreshSessionList loads the connection's sessions for the switcher.
  // Re-invoked on `session.title_changed` (subscribeEvents) so a rename
  // of the ACTIVE session — the only session whose events the page's
  // triple-scoped SSE subscription carries — refreshes the labels.
  //
  // D-424 — the chat catalog explicitly requests the `lifecycle`
  // projection: the switcher needs only id / title / last activity, and
  // the lifecycle row skips ALL counter enrichment (its counters read
  // `not_requested`; a counter-dependent filter or sort is rejected
  // `invalid_request` on a lifecycle request, so this request never asks
  // for one).
  async function refreshSessionList(): Promise<void> {
    if (client === null) return;
    try {
      const resp = await new SessionsProtocol(client).list(chatCatalogListRequest());
      sessionList = resp.rows;
    } catch {
      sessionList = [];
    }
  }

  /** The active session's title from the freshest sessionList read ("" when unset). */
  const activeTitle = $derived(sessionList.find((s) => s.session_id === sessionID)?.title ?? '');

  /** Opens the inline rename editor for the ACTIVE session (D-288). */
  function startRenameActive(): void {
    renameDraft = activeTitle;
    renameError = null;
    renamingActive = true;
  }

  function cancelRenameActive(): void {
    renamingActive = false;
    renameDraft = '';
    renameError = null;
  }

  /** Saves the draft title for the ACTIVE session via `sessions.set_title`. */
  async function saveRenameActive(): Promise<void> {
    if (client === null || renameBusy) return;
    renameBusy = true;
    renameError = null;
    try {
      await client.sessions.setTitle(sessionID, renameDraft.trim());
      await refreshSessionList();
      renamingActive = false;
      renameDraft = '';
    } catch (err) {
      renameError = err instanceof ProtocolError ? err.message : String(err);
    } finally {
      renameBusy = false;
    }
  }

  // newSession opens a fresh conversation: a new session id, materialised
  // create-on-first-use on the first send. A full navigation re-mounts the
  // page so the per-session client + subscription rebuild cleanly.
  function newSession(): void {
    const id = `sess-${crypto.randomUUID().slice(0, 12)}`;
    window.location.assign(`/playground/${id}`);
  }

  // switchSession opens an existing conversation (full navigation → the
  // per-session client rebuilds + hydratePastTurns reloads its turns).
  function switchSession(id: string): void {
    if (id && id !== sessionID) window.location.assign(`/playground/${id}`);
  }

  /* ================================================================ */
  /* Loading                                                           */
  /* ================================================================ */

  async function load(): Promise<void> {
    if (client === null) {
      status = 'disconnected';
      return;
    }
    status = 'loading';
    pageError = null;
    pageInfo = null;
    // HA-64 / D-425 — reload this conversation's prior turns with the
    // two-read open (one lifecycle inspect + one `sessions.turns.list` tail
    // page). Best-effort: empty for a brand-new session id (the inspect
    // answers not_found) and empty for a pre-restart session (the projection
    // has no rows — see docs/notes/session-model-contract.md).
    //
    // HA-64 / D-425 (P1) — the SSE subscription is the LIVE stream, not a
    // third projection read: the durable page is folded (and its rendered
    // running/paused task membership established) BEFORE the EventSource
    // opens, and the fold's `live_resume_seq` seeds the stream's initial
    // replay cursor so a terminal event that landed between the snapshot
    // and the physical subscription is replayed, never lost.
    const resumeSeq = await hydratePastTurns();
    subscribeEvents(client, resumeSeq);
    try {
      // Round-8 F1 / phase 84a — gate the topology probe behind the
      // runtime's advertised capabilities. A planner/RunLoop runtime
      // (the dev posture) omits `topology_snapshot` from
      // `runtime.info.capabilities`; we short-circuit to the info
      // banner without making the fetch, so the browser network log
      // stays clean. The Phase 83w-F5 / D-164 `unknown_method` catch
      // below remains the safety net for runtimes that advertise the
      // capability but reject at the wire.
      const caps = await client.capabilities();
      if (!caps.has('topology_snapshot')) {
        pageInfo = {
          headline: 'Topology view not available on this Runtime',
          detail:
            'This runtime is planner/RunLoop-shaped, not engine-graph-shaped. See docs/CONFIG.md for runtime shapes.'
        };
        // Round-6 F6 — never route the Playground main column through
        // PageState's `empty` branch. ChatPanel owns the "no messages
        // yet" copy + the composer.
        status = 'ready';
        return;
      }
      // The Playground opens against a live session — V1 starts with an
      // empty stream and grows as the operator sends messages. The
      // initial load proves the connection + Protocol surface are live
      // by fetching the topology snapshot (also feeds the trace toggle).
      await client.topology.snapshot<TopologyProjection>();
      status = 'ready';
    } catch (err) {
      // Phase 83w-F5 / D-164 — `topology.snapshot` returning
      // `unknown_method` is not an error: this Runtime is planner/
      // RunLoop-shaped and has no engine graph. The chat surface still
      // works, so the page proceeds to ready — the trace toggle is the
      // surface that now surfaces the friendly "no topology" message
      // when the operator toggles it on (see toggleTrace below).
      if (isUnknownMethod(err)) {
        pageInfo = {
          headline: 'Topology view not available on this Runtime',
          detail:
            'This runtime is planner/RunLoop-shaped, not engine-graph-shaped. See docs/CONFIG.md for runtime shapes.'
        };
        status = 'ready';
      } else {
        pageError = toError(err);
        status = 'error';
      }
    }
  }

  /* ================================================================ */
  /* Chat actions                                                      */
  /* ================================================================ */

  async function sendMessage(
    text: string,
    artifactIDs: string[],
    mode?: 'queue' | 'steer',
    dispositions?: Record<string, string>
  ): Promise<void> {
    if (chatClient === null) {
      return;
    }
    sending = true;
    running = true;
    // 108a-D — reset the live tokens/sec tracker for the new turn.
    streamChars = 0;
    streamStartMs = 0;
    tokensPerSec = 0;
    if (sessionStartedAt === null) {
      sessionStartedAt = new Date().toISOString();
    }
    const userMsg: ChatMessage = {
      id: `m-${Date.now()}-u`,
      role: 'user',
      text,
      at: new Date().toISOString(),
      artifacts: recentArtifacts
        .filter((a) => artifactIDs.includes(a.id))
        .map((a) => ({ id: a.id, mime: a.mime, filename: a.filename, sizeBytes: a.sizeBytes }))
    };
    messages = [...messages, userMsg];
    status = 'ready';
    try {
      const resp = await chatClient.sendMessage(text, artifactIDs, mode, dispositions);
      // Phase 106 (V1.2) — append an empty pending agent bubble.
      // The task.completed SSE handler populates the text from the
      // actual LLM answer when the task finishes.
      messages = [
        ...messages,
        {
          id: `m-${Date.now()}-a`,
          role: 'agent',
          text: '',
          taskID: resp.taskID,
          pending: true,
          at: new Date().toISOString()
        }
      ];
    } catch (err) {
      const e = toError(err);
      // A closed session RE-ACTIVATES transparently on send (D-312) — no
      // special handling needed. An ERASED session is terminal: the runtime
      // returns the machine-branchable `session_erased` code (never the
      // advisory message), so route the operator to start a fresh
      // conversation rather than showing a raw error.
      const text =
        e.code === 'session_erased'
          ? 'This conversation was permanently deleted and cannot be reopened — start a new one.'
          : `Send failed — ${e.code}: ${e.message}`;
      messages = [
        ...messages,
        {
          id: `m-${Date.now()}-sys`,
          role: 'system',
          text,
          at: new Date().toISOString()
        }
      ];
    } finally {
      sending = false;
    }
  }

  async function applyOverrides(overrides: {
    reasoningEffort?: string;
    temperature?: number;
    maxTokens?: number;
    systemPromptOverride?: string;
  }): Promise<void> {
    if (chatClient === null) {
      return;
    }
    overridesPending = true;
    overridesResult = null;
    try {
      await chatClient.setOverrides(overrides);
      overridesResult = { ok: true, message: 'Override applied to the next message.' };
    } catch (err) {
      const e = toError(err);
      overridesResult = { ok: false, message: `${e.code}: ${e.message}` };
    } finally {
      overridesPending = false;
    }
  }

  // Pause / resume the active run via the SHIPPED pause/resume control
  // verbs (Phase 54). Toggles on `paused`; no-op when no run is in flight.
  async function pauseRun(): Promise<void> {
    if (client === null || activeTaskID === null) {
      return;
    }
    try {
      if (paused) {
        await client.control.resume(activeTaskID);
        paused = false;
      } else {
        await client.control.pause(activeTaskID);
        paused = true;
      }
    } catch (err) {
      const e = toError(err);
      messages = [
        ...messages,
        {
          id: `m-${Date.now()}-sys`,
          role: 'system',
          text: `${paused ? 'Resume' : 'Pause'} failed — ${e.code}: ${e.message}`,
          at: new Date().toISOString()
        }
      ];
    }
  }

  async function cancelRun(): Promise<void> {
    if (chatClient === null) {
      return;
    }
    try {
      await chatClient.cancelRun(false);
      running = false;
      paused = false;
    } catch (err) {
      const e = toError(err);
      messages = [
        ...messages,
        {
          id: `m-${Date.now()}-sys`,
          role: 'system',
          text: `Cancel failed — ${e.code}: ${e.message}`,
          at: new Date().toISOString()
        }
      ];
    }
  }

  async function restartRun(): Promise<void> {
    if (chatClient === null) {
      return;
    }
    try {
      await chatClient.restartRun();
      messages = [];
      running = true;
      // Round-6 F6 — keep status === 'ready' so ChatPanel renders the
      // composer; ChatPanel owns the "No messages yet" copy on an
      // empty stream.
      status = 'ready';
    } catch (err) {
      const e = toError(err);
      messages = [
        ...messages,
        {
          id: `m-${Date.now()}-sys`,
          role: 'system',
          text: `Restart failed — ${e.code}: ${e.message}`,
          at: new Date().toISOString()
        }
      ];
    }
  }

  async function decideIntervention(runID: string, pauseToken: string, approve: boolean): Promise<void> {
    if (chatClient === null) {
      return;
    }
    try {
      if (approve) {
        await chatClient.approveIntervention(runID, pauseToken);
      } else {
        await chatClient.rejectIntervention(runID, pauseToken);
      }
      interventions = interventions.filter((i) => i.pauseToken !== pauseToken);
    } catch (err) {
      const e = toError(err);
      messages = [
        ...messages,
        {
          id: `m-${Date.now()}-sys`,
          role: 'system',
          text: `Intervention ${approve ? 'approve' : 'reject'} failed — ${e.code}: ${e.message}`,
          at: new Date().toISOString()
        }
      ];
    }
  }

  /* ================================================================ */
  /* Trace toggle (Phase 74 topology.snapshot)                         */
  /* ================================================================ */

  async function toggleTrace(next: boolean): Promise<void> {
    traceOn = next;
    if (!next || client === null) {
      return;
    }
    traceLoading = true;
    traceError = '';
    try {
      // Round-8 F1 / phase 84a: gate the on-demand topology fetch
      // behind the runtime's advertised capabilities so the trace
      // toggle doesn't fire a wire request on a planner/RunLoop
      // runtime that can't answer. The D-164 unknown_method catch
      // below stays as the safety net.
      const caps = await client.capabilities();
      if (!caps.has('topology_snapshot')) {
        traceError = 'Topology view not available on this Runtime (planner/RunLoop runtime).';
        traceNodes = [];
        return;
      }
      const proj = await client.topology.snapshot<TopologyProjection>();
      traceNodes = proj.nodes.map((n) => ({ id: n.name, kind: n.kind }));
    } catch (err) {
      // Phase 83w-F5 / D-164 — `unknown_method` on this Runtime is the
      // friendly "no engine graph" case, not a failure.
      if (isUnknownMethod(err)) {
        traceError = 'Topology view not available on this Runtime (planner/RunLoop runtime).';
      } else {
        traceError = toError(err).message;
      }
      traceNodes = [];
    } finally {
      traceLoading = false;
    }
  }

  function previewArtifact(id: string): void {
    // Surface the artifact as a system message carrying the reference —
    // the ArtifactReferenceCard renders the preview by reference (D-026).
    const a = recentArtifacts.find((x) => x.id === id);
    if (a === undefined) {
      return;
    }
    messages = [
      ...messages,
      {
        id: `m-${Date.now()}-art`,
        role: 'system',
        text: `Artifact preview · ${a.filename}`,
        at: new Date().toISOString(),
        artifacts: [{ id: a.id, mime: a.mime, filename: a.filename, sizeBytes: a.sizeBytes }]
      }
    ];
    status = 'ready';
  }

  /* ================================================================ */
  /* Saved views (Console-DB-backed, D-061)                            */
  /* ================================================================ */

  async function refreshSavedViews(): Promise<void> {
    if (savedFilters === null) {
      return;
    }
    try {
      const records = await savedFilters.list();
      savedViews = records.map((r) => ({ id: r.id, name: r.name }));
      savedSpecs = new Map(records.map((r) => [r.id, r.viewSpec]));
    } catch {
      savedViews = [];
      savedSpecs = new Map();
    }
  }

  function applySavedView(id: string): void {
    const spec = savedSpecs.get(id);
    if (spec === undefined) {
      return;
    }
    activeSavedId = id;
    traceOn = spec.traceOn ?? false;
  }

  async function deleteSavedView(id: string): Promise<void> {
    if (savedFilters === null) {
      return;
    }
    await savedFilters.delete(id);
    if (activeSavedId === id) {
      activeSavedId = null;
    }
    await refreshSavedViews();
  }

  async function saveCurrentView(): Promise<void> {
    const name = saveName.trim();
    if (name.length === 0 || savedFilters === null) {
      return;
    }
    const created = await savedFilters.create(name, { traceOn });
    saveName = '';
    await refreshSavedViews();
    activeSavedId = created.id;
  }

  /* ================================================================ */
  /* Boot                                                              */
  /* ================================================================ */

  onMount(() => {
    const base = resolveConnection();
    if (base === null) {
      connection = null;
      client = null;
      status = 'disconnected';
      return;
    }
    // D-171 — the connection token is a per-backend credential; THIS page
    // operates the conversation session from the URL (`session_id`). Build
    // a per-session connection so every RPC carries `X-Harbor-Session` =
    // the conversation id and the SSE subscribes scoped to it. A fresh
    // session id (e.g. from "New session") is materialised create-on-
    // first-use on the first send. tenant + user stay token-verified.
    connection = { ...base, identity: { ...base.identity, session: sessionID || base.identity.session } };
    client = injectedClient ?? new HarborClient({ connection });
    canControl = hasScope(connection, 'admin');
    chatClient = buildChatClient(client);
    // The §4.4 adapter that routes app→host requests through the Harbor
    // Protocol client → Runtime → MCP southbound (D-173). Injected into the
    // hosted App panels so a fullscreen / pip app stays inside the identity
    // boundary and the unified approval / OAuth gates.
    appHostClient = makeMCPAppHostClient(client);
    void refreshSessionList();
    void refreshArtifacts();
    void refreshTools();

    // Resolve the real Protocol version + agent display name. Both are
    // best-effort: runtime.info is universally advertised; the agent
    // registry may be empty (the dev posture registers no named agent),
    // in which case the honest 'default agent' fallback stands
    // (AC-11 fallback chain).
    void (async () => {
      try {
        const info = await client!.posture.info<{ display_name?: string }>();
        // Fallback rung 3 (below the address-book name + agents.list): the
        // runtime's own display name. Only used if nothing better resolved.
        if (info.display_name && activeAgent === 'default agent') activeAgent = info.display_name;
      } catch {
        /* keep the em-dash */
      }
      try {
        const list = await client!.agents.list<{ agents?: Array<{ name?: string }> }>();
        const name = list.agents?.[0]?.name;
        // Fallback rung 2 — only when the address book has not named it.
        if (name && (activeAgent === 'default agent' || activeAgent === 'harbor dev')) {
          activeAgent = name;
        }
      } catch {
        /* keep 'default agent' */
      }
    })();

    void (async () => {
      try {
        const db = await openListPageDB(connection!);
        const operator = await operatorIdOf(
          connection!.identity.tenant,
          connection!.identity.user
        );
        savedFilters = new PlaygroundSavedFilters(db, operator);
        await refreshSavedViews();
        // F1 — the authoritative display name is the one the operator typed
        // in Settings → Connected Runtimes (the Console DB address book),
        // matched to the active connection's base URL. It wins over the
        // agents.list / runtime.info fallbacks.
        try {
          const runtimes = await db.runtimes.list(operator);
          const hit = runtimes.find((r) => r.base_url === connection!.baseURL);
          if (hit?.name) activeAgent = hit.name;
        } catch {
          /* address-book name is best-effort */
        }
      } catch {
        savedFilters = null;
      }
    })();

    void load();
  });

  onDestroy(() => {
    if (taskEvents !== null) {
      taskEvents.close();
      taskEvents = null;
    }
  });
</script>

<svelte:head>
  <title>Playground · {sessionID} · Harbor Console</title>
</svelte:head>

<div class="page" data-testid="playground-page" data-session-id={sessionID}>
  <!-- 108a — the agent sub-bar is the page's top row; the shell breadcrumb
       already names "Playground", so the bulky PageHeader title/subtitle is
       dropped to reclaim vertical space (mock Image 3). -->
  <PlaygroundHeader
    activeAgent={activeAgent}
    sessionID={sessionID}
    model={modelName}
    planner={plannerName}
    running={running}
    paused={paused}
    phase={runPhase}
    canImpersonate={canControl}
    impersonationTargets={impersonationTargets}
    activeImpersonation={activeImpersonation}
    onagentchange={(a) => (activeAgent = a)}
    oncancel={() => void cancelRun()}
    onpause={() => void pauseRun()}
    onrestart={() => void restartRun()}
    onimpersonate={(t) => (activeImpersonation = t)}
  />

  <!-- 108a — one compact toolbar row: the D-171 conversation switcher +
       New session, plus saved views (chips only when present, so the
       default state reclaims the row). -->
  <div class="toolbar-row" data-testid="playground-session-strip">
    <label class="session-pick">
      <span class="session-pick-label">Conversation</span>
      <select
        class="session-select mono"
        data-testid="playground-session-select"
        value={sessionID}
        onchange={(e) => switchSession((e.currentTarget as HTMLSelectElement).value)}
      >
        {#if !sessionList.some((s) => s.session_id === sessionID)}
          <option value={sessionID}>{activeTitle || sessionID || '—'}</option>
        {/if}
        {#each sessionList as s (s.session_id)}
          <option value={s.session_id}>{s.title || s.session_id}</option>
        {/each}
      </select>
    </label>

    {#if renamingActive}
      <span class="rename-inline">
        <input
          type="text"
          class="rename-input"
          data-testid="playground-rename-input"
          bind:value={renameDraft}
          maxlength={MAX_SESSION_TITLE_LEN}
          disabled={renameBusy}
          onkeydown={(e) => {
            if (e.key === 'Enter') void saveRenameActive();
            else if (e.key === 'Escape') cancelRenameActive();
          }}
        />
        <button
          type="button"
          class="session-new small"
          data-testid="playground-rename-save"
          disabled={renameBusy}
          onclick={() => void saveRenameActive()}
        >
          Save
        </button>
        <button
          type="button"
          class="session-new small ghost"
          data-testid="playground-rename-cancel"
          disabled={renameBusy}
          onclick={cancelRenameActive}
        >
          Cancel
        </button>
        {#if renameError}
          <span class="rename-error" data-testid="playground-rename-error">{renameError}</span>
        {/if}
      </span>
    {:else}
      <button
        type="button"
        class="session-new small"
        data-testid="playground-rename-start"
        title={activeTitle ? 'Rename this conversation' : 'Add a title for this conversation'}
        onclick={startRenameActive}
      >
        Rename
      </button>
    {/if}

    <button
      type="button"
      class="session-new"
      data-testid="playground-new-session"
      onclick={newSession}
    >
      + New session
    </button>

    {#if savedFilters !== null}
      <div class="toolbar-views">
        {#if savedViews.length > 0}
          <SavedViewChips
            views={savedViews}
            activeId={activeSavedId}
            onselect={applySavedView}
            ondelete={(id) => void deleteSavedView(id)}
          />
        {/if}
        <input
          class="view-save-input"
          type="text"
          placeholder="Save current as…"
          bind:value={saveName}
          data-testid="playground-save-name"
          onkeydown={(e) => e.key === 'Enter' && void saveCurrentView()}
        />
        <button
          type="button"
          class="view-save-btn"
          data-testid="playground-save-view"
          disabled={saveName.trim().length === 0}
          onclick={() => void saveCurrentView()}
        >
          Save view
        </button>
      </div>
    {/if}
  </div>

  <!-- Phase 108a KPI strip — the integrated metadata band -->
  <KpiStrip
    sessionID={sessionID}
    startedAt={sessionStartedAt}
    activeWorkMs={activeWorkMs}
    activeSinceMs={activeTurnStartMs}
    identityUser={connection?.identity.user ?? ''}
    identityTenant={connection?.identity.tenant ?? ''}
    scopeLabel={connection?.scopes.includes('admin') ? 'admin' : (connection?.scopes[0] ?? '')}
    tokenCount={tokenCount}
    promptTokens={promptTokens}
    outputTokens={outputTokens}
    tokenSamples={tokenSamples}
    costUSD={costUSD}
    ceilingUSD={ceilingUSD}
    hasCostReading={hasCostReading}
    turnLatencies={turnLatencies}
  />

  <!-- The chat + composer column. Hoisted into a snippet so the same chat
       surface composes into the default region, a fullscreen Chat tab, and the
       pip split's left pane — never re-instantiated, so switching DisplayMode
       does NOT reload the session (D-062). -->
  {#snippet chatColumn()}
    <div class="main-col">
      <PageState status={status} error={pageError} info={pageInfo} onretry={() => void load()}>
        {#snippet skeleton()}
          <div class="chat-skeleton" aria-hidden="true"></div>
        {/snippet}
        {#snippet empty()}
          <div class="empty-block" data-testid="playground-empty">
            <p class="headline">No messages yet</p>
            <p class="detail">Send a message below to start the conversation.</p>
          </div>
        {/snippet}

        {#if historyNotice !== null}
          <p class="history-notice" data-testid="history-notice" role="status">
            {historyNotice}
          </p>
        {/if}
        {#if turnProjectionUnavailable}
          <!-- The explicit, user-invoked degraded/forensic fallback (D-425).
               The runtime predates the `sessions.turns.*` surface; the normal
               open does NOT silently fall back to forensic event replay. The
               operator opts in, and the control names the degradation. -->
          <div class="forensic-fallback" data-testid="forensic-fallback">
            <p class="history-notice" role="status">
              Conversation turn projection unavailable on this Runtime.
            </p>
            <button
              type="button"
              class="session-new small"
              data-testid="forensic-reopen-button"
              onclick={() => {
                legacyReopenRequested = true;
                void hydratePastTurnsLegacy();
              }}
            >
              {legacyReopenRequested ? 'Reopening via forensic event replay…' : 'Reopen via forensic event replay (degraded)'}
            </button>
          </div>
        {:else if turnPages.length > 0}
          <!-- Older-page paging rides the opaque snapshot/keyset cursor
               (`next_older_cursor`) — stable ordering under append, typed
               refusal surfaced honestly (never a silent reset). -->
          {#if turnPages[turnPages.length - 1]?.hasMore}
            <div class="older-turns-row">
              <button
                type="button"
                class="session-new small"
                data-testid="load-older-turns"
                disabled={olderLoading}
                onclick={() => void loadOlderTurns()}
              >
                {olderLoading ? 'Loading older turns…' : 'Load older turns'}
              </button>
            </div>
          {/if}
          {#if olderError !== null}
            <p class="history-notice older-error" data-testid="older-turns-error" role="status">
              {olderError}
            </p>
          {/if}
        {/if}
        {#if chatClient !== null}
          <ChatPanel
            messages={pagedMessages}
            client={chatClient}
            sending={sending}
            running={activeTaskID !== null}
            onsend={(text, ids, mode) => void sendMessage(text, ids, mode)}
            appHostClient={appHostClient ?? undefined}
            availableDisplayModes={APP_DISPLAY_MODES}
            onAppDisplayModeRequest={onInlineAppDisplayModeRequest}
          />
        {/if}
      </PageState>

      <!-- 108a composer telemetry (mock Image 13 bottom strip) — page-level
           live metrics under the composer. Context window % lands once the
           runtime exposes the model context-window (R2); until then the
           absolute context size is shown. -->
      <div class="composer-telemetry" data-testid="composer-telemetry">
        <span class="tel-phase" data-phase={runPhase}>
          {runPhase === 'streaming' ? '● Streaming' : runPhase === 'active' ? '● Active' : '○ Idle'}
        </span>
        {#if tokensPerSec > 0 && runPhase === 'streaming'}
          <span class="tel-sep">·</span>
          <span class="tabular">Tokens/sec: {tokensPerSec.toFixed(1)}</span>
        {/if}
        {#if lastPromptTokens > 0}
          <span class="tel-sep">·</span>
          <span class="tabular">
            Context: {(lastPromptTokens / 1000).toFixed(1)}k{#if contextWindow > 0} / {(contextWindow / 1000).toFixed(0)}k ({Math.round((lastPromptTokens / contextWindow) * 100)}%){/if}
          </span>
        {/if}
        <span class="tel-spacer"></span>
        <span class="tel-live">
          <span class="tel-dot" data-on={eventsStreamLive} aria-hidden="true"></span>
          Session {eventsStreamLive ? 'live' : 'off'}
        </span>
      </div>

      {#if status === 'ready' && messages.length > pageSize}
        <Pagination
          page={pageIndex}
          pageSize={pageSize}
          total={messages.length}
          onpage={(p) => (pageIndex = p)}
          onpagesize={(s) => {
            pageSize = s;
            pageIndex = 1;
          }}
        />
      {/if}
    </div>
  {/snippet}

  <!-- The right detail rail. Shown by default; hidden by default in pip with a
       toggle to reopen (the toggle never resets the split ratio — D-061). -->
  {#snippet railColumn()}
    <DetailRail>
      <RailCard title="Controls">
        <ControlsCard
          model={modelName}
          tools={toolList}
          pending={overridesPending}
          result={overridesResult}
          onapply={(o) => void applyOverrides(o)}
        />
      </RailCard>
      <RailCard title="Pending interventions">
        <PendingInterventionsCard
          interventions={interventions}
          canDecide={canControl}
          onapprove={(runID, pauseToken) => void decideIntervention(runID, pauseToken, true)}
          onreject={(runID, pauseToken) => void decideIntervention(runID, pauseToken, false)}
        />
      </RailCard>
      <RailCard title="Recent artifacts">
        <PlaygroundArtifactsCard artifacts={recentArtifacts} onpreview={previewArtifact} />
      </RailCard>
      <RailCard title="Trace">
        {#if pageInfo}
          <p class="topo-info" data-testid="playground-topology-info">{pageInfo.headline}</p>
        {/if}
        <TraceToggle
          enabled={traceOn}
          nodes={traceNodes}
          loading={traceLoading}
          error={traceError}
          ontoggle={(next) => void toggleTrace(next)}
        />
      </RailCard>
    </DetailRail>
  {/snippet}

  <!-- One MCP App panel hosting the reused 109b renderer for a page region. -->
  {#snippet appPanel(app: OpenApp)}
    {#if appHostClient !== null}
      <AppPanel
        app={app}
        appHostClient={appHostClient}
        onrequestmode={(mode) => appRequestMode(app, mode)}
        onclose={() => dispatchLayout({ type: 'close-app', id: app.id })}
      />
    {/if}
  {/snippet}

  <!-- Region routing — the DisplayMode layout state machine (D-062). The grid
       columns + rail visibility derive from `region`; `inline` apps never reach
       here (they render in the chat scroll, 109b unchanged). -->
  <div class="layout" data-region={region.region} data-rail={region.railVisible} data-testid="playground-layout">
    {#if region.region === 'fullscreen'}
      <div class="main-col fullscreen-main" data-testid="fullscreen-region">
        <AppTabStrip
          tabs={region.tabs}
          activeTabId={region.activeTabId}
          onactivate={(id) => dispatchLayout({ type: 'activate-tab', id })}
          onclose={(id) => dispatchLayout({ type: 'close-app', id })}
        />
        <div class="fullscreen-body">
          {#if region.fullscreenApp !== null}
            {@render appPanel(region.fullscreenApp)}
          {:else}
            {@render chatColumn()}
          {/if}
        </div>
      </div>
      {@render railColumn()}
    {:else if region.region === 'pip'}
      <div class="pip-region" data-testid="pip-region">
        <div class="pip-bar">
          <button
            type="button"
            class="rail-toggle"
            data-testid="pip-rail-toggle"
            aria-pressed={region.railVisible}
            onclick={() => dispatchLayout({ type: 'toggle-rail' })}
          >
            {region.railVisible ? 'Hide rail' : 'Show rail'}
          </button>
        </div>
        <SplitPane
          ratio={region.splitRatio}
          onratio={(r) => dispatchLayout({ type: 'set-ratio', ratio: r })}
        >
          {#snippet left()}
            {@render chatColumn()}
          {/snippet}
          {#snippet right()}
            {#if region.pipApp !== null}
              {@render appPanel(region.pipApp)}
            {/if}
          {/snippet}
        </SplitPane>
      </div>
      {#if region.railVisible}
        {@render railColumn()}
      {/if}
    {:else}
      {@render chatColumn()}
      {@render railColumn()}
    {/if}
  </div>
</div>

<style>
  /* Phase 108 — the page fills the shell's content box (which already
     supplies the --space-6 padding) and owns its own internal scroll.
     The header / filter / KPI bands are fixed-height; only `.layout`
     (and inside it the chat stream + the right rail) scroll. No
     whole-document scroll. */
  .page {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    height: 100%;
    min-height: 0;
    overflow: hidden;
  }

  .layout {
    flex: 1;
    min-height: 0;
    display: grid;
    grid-template-columns: 1fr var(--size-rail);
    gap: var(--space-4);
    align-items: stretch;
  }

  /* In pip the right rail is hidden by default — a single content column. The
     toggle (`data-rail='true'`) reopens the rail without resetting the split. */
  .layout[data-region='pip'][data-rail='false'] {
    grid-template-columns: 1fr;
  }

  .fullscreen-main {
    min-width: 0;
    min-height: 0;
  }

  .fullscreen-body {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
  }

  .pip-region {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    min-width: 0;
    min-height: 0;
  }

  .pip-bar {
    display: flex;
    justify-content: flex-end;
  }

  .history-notice {
    margin: 0 var(--space-3) var(--space-2);
    padding: var(--space-1) var(--space-2);
    background: var(--color-surface-raised);
    color: var(--color-text-muted);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    font-size: var(--text-xs);
  }

  /* The explicit degraded/forensic fallback control (HA-64 / D-425) —
     shown only when the turn projection is unavailable. */
  .forensic-fallback {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: var(--space-1);
    margin: 0 var(--space-3) var(--space-2);
  }

  .forensic-fallback .history-notice {
    margin: 0;
  }

  .older-turns-row {
    display: flex;
    justify-content: center;
    margin: 0 var(--space-3) var(--space-2);
  }

  .older-error {
    color: var(--color-warning);
  }

  .rail-toggle {
    background: var(--color-surface-raised);
    color: var(--color-text-muted);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .rail-toggle:hover {
    border-color: var(--color-accent);
    color: var(--color-accent);
  }

  .main-col {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    min-width: var(--space-0);
    min-height: 0;
    /* Fill the parent's height. In the default region `.main-col` is a
       grid item stretched by `.layout` (flex-grow ignored there); inside a
       SplitPane `.pane` (pip) or `.fullscreen-body` (fullscreen) — both
       flex columns — this is what makes the chat column fill the pane so
       the conversation + composer stay visible beside the app. Without it
       the column collapses to content height and the chat disappears in
       side-by-side. */
    flex: 1;
  }

  .toolbar-row {
    display: flex;
    align-items: center;
    gap: var(--space-3);
  }

  .toolbar-views {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    margin-left: auto;
    flex-wrap: wrap;
  }

  .session-pick {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .session-pick-label {
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wider);
    color: var(--color-text-muted);
  }

  .session-select {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    max-width: var(--size-rail);
  }

  .session-new {
    background: var(--color-surface-raised);
    color: var(--color-accent);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .session-new:hover {
    border-color: var(--color-accent);
  }

  .session-new.small {
    padding: var(--space-1) var(--space-2);
  }

  .session-new.ghost {
    background: none;
    color: var(--color-text-muted);
  }

  .session-new:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }

  /* D-288 inline rename — the ACTIVE session's title editor. */
  .rename-inline {
    display: inline-flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-1);
  }

  .rename-input {
    width: var(--size-rename-input-width);
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
  }

  .rename-error {
    flex-basis: 100%;
    color: var(--color-danger);
    font-size: var(--text-xs);
  }

  .view-save-input {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    width: var(--size-chip-min-width);
  }

  .view-save-btn {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .view-save-btn:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }

  .topo-info {
    margin: var(--space-0) var(--space-0) var(--space-2);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .composer-telemetry {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .tel-phase[data-phase='streaming'] {
    color: var(--color-success);
  }

  .tel-phase[data-phase='active'] {
    color: var(--color-accent);
  }

  .tel-sep {
    opacity: 0.5;
  }

  .tel-spacer {
    flex: 1;
  }

  .tel-live {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
  }

  .tel-dot {
    width: var(--space-2);
    height: var(--space-2);
    border-radius: 50%;
    background: var(--color-text-muted);
  }

  .tel-dot[data-on='true'] {
    background: var(--color-success);
  }

  .tabular {
    font-variant-numeric: var(--font-variant-tabular);
  }

  .chat-skeleton {
    height: var(--space-12);
    background: var(--color-surface-raised);
    border-radius: var(--radius-md);
  }

  .empty-block {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-12) var(--space-4);
    text-align: center;
  }

  .empty-block .headline {
    margin: var(--space-0);
    font-size: var(--text-lg);
    font-weight: 600;
    color: var(--color-text);
  }

  .empty-block .detail {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }
</style>
