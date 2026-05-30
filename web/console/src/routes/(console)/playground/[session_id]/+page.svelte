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
  import { HarborClient, type ProtocolClient } from '$lib/protocol/harbor.js';
  import { ProtocolError, isUnknownMethod } from '$lib/protocol/errors.js';
  import type { TopologyProjection } from '$lib/protocol/topology.js';
  import { resolveConnection, hasScope, type RuntimeConnection } from '$lib/connection.js';
  import { openListPageDB } from '$lib/db/console_db.js';
  import { operatorIdOf } from '$lib/db/schema.js';
  import { parseAnswerFromDetail, parseReasoningSteps } from './answer-envelope.js';
  import type { ReasoningStep } from '$lib/chat/types.js';
  import { applyChunk, applyReasoningChunk, finalizeStream } from './chunk-stream.js';
  import {
    decodeChunk,
    decodeCost,
    decodeLifecycle,
    decodeBudget,
    decodePlannerDecision,
    type ChunkEvent,
    type CostEvent,
    type LifecycleEvent
  } from './wire-events.js';
  import type { ChatToolCall } from '$lib/chat/types.js';
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
  // ISO timestamp of the session's first turn — drives the KPI Started +
  // live Duration columns. Set on the first send; null until then.
  let sessionStartedAt = $state<string | null>(null);

  // D-171 — the connection's other conversations (sessions.list), for the
  // session switcher. One token, many sessions.
  let sessionList = $state<Array<{ session_id: string; last_activity_at?: string }>>([]);
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
      async sendMessage(text, artifactIDs, mode) {
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
          queuedSends = [...queuedSends, { text, artifactIDs }];
          return { taskID: activeTaskID };
        }
        const resp = await c.control.start<{ task_id: string }>(text, {
          description: `Playground turn · ${activeAgent}`,
          inputArtifactIDs: artifactIDs
        });
        activeTaskID = resp.task_id;
        return { taskID: resp.task_id };
      },
      async setOverrides(overrides) {
        const payload: Record<string, unknown> = { session_id: sessionID };
        if (overrides.reasoningEffort !== undefined) {
          payload.reasoning_effort = overrides.reasoningEffort;
        }
        if (overrides.temperature !== undefined) {
          payload.temperature = overrides.temperature;
        }
        if (overrides.topP !== undefined) {
          payload.top_p = overrides.topP;
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
        const resp = await c.artifacts.getRef<{ url: string }>({ id });
        return resp.url;
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
      async approveIntervention(runID) {
        await c.control.dispatch('approve', runID);
      },
      async rejectIntervention(runID) {
        await c.control.dispatch('reject', runID);
      }
    };
  }

  let chatClient = $state<ChatProtocolClient | null>(null);

  /* ================================================================ */
  /* Round-6 F10 — active-task lifecycle + queued-send drain           */
  /* ================================================================ */

  // The task id of the currently-running foreground run (null when no
  // run is in flight). `<ChatComposer>` reads this via the `running`
  // prop to decide whether to show the queue-vs-steer mode picker; the
  // sendMessage adapter consults it to route the message correctly.
  let activeTaskID = $state<string | null>(null);

  // FIFO queue of "send when current run terminates" messages. The
  // lifecycle watcher below drains the queue with `start` calls as
  // soon as activeTaskID becomes null.
  let queuedSends = $state<Array<{ text: string; artifactIDs: string[] }>>([]);

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

  // recordTurn pushes a completed turn's wall-clock latency (the
  // `tasks.get` duration_ms) into the p50 buffer.
  function recordTurn(durationMs: number): void {
    if (durationMs <= 0) return;
    const next = [...turnLatencies, durationMs];
    turnLatencies = next.length > LATENCY_SAMPLE_CAP ? next.slice(-LATENCY_SAMPLE_CAP) : next;
  }

  // handleChunk streams one decoded `llm.completion.chunk` into the
  // pending agent bubble. Only the `content` channel grows the answer
  // body; `reasoning` deltas land in the accordion at completion, not
  // inline. Done flips the bubble's streaming flag off.
  function handleChunk(ev: ChunkEvent): void {
    if (ev.taskID !== activeTaskID) return;
    if (ev.delta !== '') {
      // The content channel grows the answer body; the reasoning channel
      // grows the live "Reasoning" disclosure (108a — runtime reasoning
      // emit fixed in the corrections layer). Neither pollutes the other.
      if (ev.kind === 'reasoning') {
        messages = applyReasoningChunk(messages, ev.taskID, ev.delta);
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

  // handleTerminal reconciles a completed/failed/cancelled turn. On
  // completion it fetches the authoritative answer + reasoning trace via
  // `tasks.get` (Phase 106) and records the turn latency; on
  // failure/cancellation it converts the bubble to a system error.
  async function handleTerminal(ev: LifecycleEvent): Promise<void> {
    if (ev.taskID !== activeTaskID) return;
    const taskID = ev.taskID;
    if (ev.kind === 'completed' && client !== null) {
      try {
        const detail = await client.tasks.get<{
          result_inline?: string;
          trajectory?: { steps?: ReasoningStep[] };
          task?: { duration_ms?: number };
        }>(taskID);
        const answer = parseAnswerFromDetail(detail);
        const reasoningSteps = parseReasoningSteps(detail);
        const durationMs = detail.task?.duration_ms ?? 0;
        recordTurn(durationMs);
        const tc = turnCost[taskID];
        messages = messages.map((m) =>
          m.taskID === taskID && m.role === 'agent'
            ? {
                ...m,
                text: answer,
                reasoningSteps: reasoningSteps.length > 0 ? reasoningSteps : undefined,
                toolCalls:
                  (turnTools[taskID] ?? []).length > 0
                    ? turnTools[taskID].map((t) => ({ ...t, status: 'succeeded' as const }))
                    : undefined,
                meta: {
                  elapsedMs: durationMs > 0 ? durationMs : undefined,
                  tokens: tc?.tokens,
                  costUSD: tc?.cost
                },
                pending: false,
                streaming: false
              }
            : m
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
      messages = messages.map((m) =>
        m.taskID === taskID && m.role === 'agent'
          ? { ...m, text: errorText, role: 'system', pending: false, streaming: false }
          : m
      );
    }
    running = false;
    paused = false;
    activeTaskID = null;
    void drainQueue();
  }

  // Best-effort EventSource subscription. The bus auto-scopes to the
  // bearer's (tenant, user, session), so the page receives only its own
  // session's events. Every frame is the flat `wireEvent` projection
  // (`{type, payload:{...PascalCase}}`); the `wire-events.ts` decoders
  // own that shape (the prior cut read top-level snake_case and silently
  // dropped every chunk). The subscription is optional — a runtime
  // without the SSE surface leaves the stream Off and the page still
  // works (the operator sends manually; answers arrive via tasks.get).
  function subscribeEvents(c: ProtocolClient): void {
    try {
      const url = c.events.subscribeURL({
        eventTypes: [
          'task.completed',
          'task.failed',
          'task.cancelled',
          'llm.completion.chunk',
          'llm.cost.recorded',
          'governance.budget_exceeded',
          'planner.decision'
        ]
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
      const onTerminal = (msg: MessageEvent): void => {
        const ev = decodeLifecycle((msg as MessageEvent<string>).data);
        if (ev !== null) void handleTerminal(ev);
      };
      es.addEventListener('task.completed', onTerminal);
      es.addEventListener('task.failed', onTerminal);
      es.addEventListener('task.cancelled', onTerminal);
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
      await sendMessage(next.text, next.artifactIDs);
    } catch {
      // Errors surface through the page's existing sendMessage error
      // path; no retry here to avoid burying the operator's intent.
    }
  }

  /* ================================================================ */
  /* Session reload (D-171)                                            */
  /* ================================================================ */

  // hydratePastTurns reloads the conversation's prior turns when opening
  // an existing session (the per-session client scopes tasks.list to the
  // URL session). Each completed task becomes a user bubble (its query) +
  // an agent bubble (its result_inline answer). No-op when the stream is
  // already populated (a live conversation) or the session has no tasks.
  async function hydratePastTurns(): Promise<void> {
    if (client === null || messages.length > 0) return;
    try {
      const resp = await client.tasks.list<{
        rows?: Array<{ id: string; query?: string; status?: string; started_at?: string }>;
      }>({ filter: {}, page_size: 50 });
      const rows = [...(resp.rows ?? [])].sort((a, b) =>
        (a.started_at ?? '').localeCompare(b.started_at ?? '')
      );
      const hydrated: ChatMessage[] = [];
      for (const row of rows) {
        const at = row.started_at ?? new Date().toISOString();
        if (row.query) {
          hydrated.push({ id: `h-${row.id}-u`, role: 'user', text: row.query, at });
        }
        if (row.status === 'complete') {
          try {
            const detail = await client.tasks.get<{
              result_inline?: string;
              trajectory?: { steps?: ReasoningStep[] };
            }>(row.id);
            const reasoningSteps = parseReasoningSteps(detail);
            hydrated.push({
              id: `h-${row.id}-a`,
              role: 'agent',
              text: parseAnswerFromDetail(detail),
              taskID: row.id,
              at,
              reasoningSteps: reasoningSteps.length > 0 ? reasoningSteps : undefined
            });
          } catch {
            /* skip an unreadable task — best-effort reload */
          }
        }
      }
      if (hydrated.length > 0 && messages.length === 0) {
        messages = hydrated;
        if (sessionStartedAt === null && hydrated[0]?.at) sessionStartedAt = hydrated[0].at;
      }
    } catch {
      /* reload is best-effort; a runtime without tasks.list just starts empty */
    }
  }

  // refreshSessionList loads the connection's sessions for the switcher.
  async function refreshSessionList(): Promise<void> {
    if (client === null) return;
    try {
      const resp = await client.sessions.list<{
        rows?: Array<{ session_id: string; last_activity_at?: string }>;
      }>({ filter: {}, limit: 50 });
      sessionList = resp.rows ?? [];
    } catch {
      sessionList = [];
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
    // D-171 — reload this conversation's prior turns (sessions persist via
    // the catalog; tasks.list is scoped to the URL session by the
    // per-session client). Best-effort: empty for a brand-new session id,
    // and empty for a pre-restart session (task history is in-memory — see
    // docs/notes/session-model-contract.md).
    await hydratePastTurns();
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
    mode?: 'queue' | 'steer'
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
      const resp = await chatClient.sendMessage(text, artifactIDs, mode);
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
      messages = [
        ...messages,
        {
          id: `m-${Date.now()}-sys`,
          role: 'system',
          text: `Send failed — ${e.code}: ${e.message}`,
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
    topP?: number;
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

  async function decideIntervention(runID: string, approve: boolean): Promise<void> {
    if (chatClient === null) {
      return;
    }
    try {
      if (approve) {
        await chatClient.approveIntervention(runID);
      } else {
        await chatClient.rejectIntervention(runID);
      }
      interventions = interventions.filter((i) => i.runID !== runID);
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
    subscribeEvents(client);
    void refreshSessionList();

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
          <option value={sessionID}>{sessionID || '—'}</option>
        {/if}
        {#each sessionList as s (s.session_id)}
          <option value={s.session_id}>{s.session_id}</option>
        {/each}
      </select>
    </label>
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

  <div class="layout">
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

        {#if chatClient !== null}
          <ChatPanel
            messages={pagedMessages}
            client={chatClient}
            sending={sending}
            running={activeTaskID !== null}
            onsend={(text, ids, mode) => void sendMessage(text, ids, mode)}
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
        {#if hasCostReading}
          <span class="tel-sep">·</span>
          <span class="tabular">
            Cost: ${costUSD.toFixed(4)}{ceilingUSD !== null ? ` / $${ceilingUSD.toFixed(2)}` : ''}
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

    <DetailRail>
      <RailCard title="Controls">
        <ControlsCard
          pending={overridesPending}
          result={overridesResult}
          onapply={(o) => void applyOverrides(o)}
        />
      </RailCard>
      <RailCard title="Pending interventions">
        <PendingInterventionsCard
          interventions={interventions}
          canDecide={canControl}
          onapprove={(runID) => void decideIntervention(runID, true)}
          onreject={(runID) => void decideIntervention(runID, false)}
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

  .main-col {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    min-width: var(--space-0);
    min-height: 0;
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
