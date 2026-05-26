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
  import PageHeader from '$lib/components/ui/PageHeader.svelte';
  import FilterBar from '$lib/components/ui/FilterBar.svelte';
  import SavedViewChips, { type SavedView } from '$lib/components/ui/SavedViewChips.svelte';
  import DetailRail from '$lib/components/ui/DetailRail.svelte';
  import RailCard from '$lib/components/ui/RailCard.svelte';
  import Pagination from '$lib/components/ui/Pagination.svelte';
  // ConnectionFooter is rendered ONCE by the app shell
  // ((console)/+layout.svelte — CONVENTIONS.md §2). The per-page import was
  // duplicating the footer (post-83k walkthrough N2); removed.
  import PageState, { type PageStatus } from '$lib/components/ui/PageState.svelte';
  import PlaygroundHeader, {
    type ImpersonationTarget
  } from '$lib/components/playground/Header.svelte';
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
  import { parseAnswerFromDetail, normalizeLifecycleType } from './answer-envelope.js';
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
  const agents = ['default agent'];
  let activeAgent = $state('default agent');
  let tokenCount = $state(0);
  let costUSD = $state(0);
  let running = $state(false);

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

  /* ---- footer constants ------------------------------------------- */
  const PROTOCOL_VERSION = 'v1';
  const CONSOLE_VERSION = 'dev';

  /* ================================================================ */
  /* Derived                                                           */
  /* ================================================================ */

  const model = $derived('runtime-default');

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
        //     The lifecycle watcher below (subscribeTaskLifecycle)
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

  // Best-effort EventSource subscription for task lifecycle. Filters
  // to the terminal task events scoped by this session's identity; the
  // bus auto-scopes to the bearer's (tenant, user, session) so the
  // page receives only its own session's events. The subscription is
  // optional — if the runtime returns 404/unknown_method (a build
  // without SSE wiring), the page degrades gracefully (the queue
  // simply does not auto-drain; the operator can still send by
  // pressing Send manually after the run completes).
  let taskEvents = $state<EventSource | null>(null);

  function subscribeTaskLifecycle(c: ProtocolClient): void {
    try {
      const url = c.events.subscribeURL({
        eventTypes: ['task.completed', 'task.failed', 'task.cancelled']
      });
      const es = new EventSource(url);
      // The bus envelope's `payload.TaskID` carries the task id for
      // task.{spawned,started,completed,failed,cancelled} events; the
      // top-level `run` carries the same id once the run loop is alive.
      // Both are checked so the watcher works whether the lifecycle
      // event lands in pre- or post-run-loop form.
      type LifecycleEvent = {
        type?: string;
        run?: string;
        payload?: { TaskID?: string };
      };
      const isTerminal = (t: string | undefined): boolean =>
        t === 'task.completed' || t === 'task.failed' || t === 'task.cancelled';
      es.addEventListener('task.completed', (msg: MessageEvent) => {
        handleLifecycle(msg, 'completed');
      });
      es.addEventListener('task.failed', (msg: MessageEvent) => {
        handleLifecycle(msg, 'failed');
      });
      es.addEventListener('task.cancelled', (msg: MessageEvent) => {
        handleLifecycle(msg, 'cancelled');
      });
      es.onmessage = (msg: MessageEvent) => {
        let parsed: LifecycleEvent | null = null;
        try {
          parsed = JSON.parse((msg as MessageEvent<string>).data) as LifecycleEvent;
        } catch {
          return;
        }
        if (parsed === null || !isTerminal(parsed.type)) {
          return;
        }
        // Normalize 'task.<x>' → '<x>' so the handler's eventType branches
        // ('completed' vs 'failed' / 'cancelled') match the named-listener
        // path; otherwise an onmessage-delivered completion would slip into
        // the error branch.
        handleLifecycle(msg, normalizeLifecycleType(parsed.type as string));
      };
      async function handleLifecycle(msg: MessageEvent, eventType: string): Promise<void> {
        let parsed: LifecycleEvent | null = null;
        try {
          parsed = JSON.parse((msg as MessageEvent<string>).data) as LifecycleEvent;
        } catch {
          return;
        }
        if (parsed === null) {
          return;
        }
        const ranTaskID = parsed.payload?.TaskID ?? parsed.run ?? '';
        if (ranTaskID === '' || ranTaskID !== activeTaskID) {
          return;
        }
        const taskID = ranTaskID;

        // Phase 106 (V1.2) — fetch the real answer from tasks.get
        // and replace the pending agent bubble. The wire shape is
        // TaskDetail (internal/protocol/types/tasks.go:409): `result_inline`
        // is at the TOP LEVEL of the response, NOT nested inside `task`
        // (that's the compact TaskRow projection).
        if (eventType === 'completed' && client !== null) {
          try {
            const detail = await client.tasks.get<{ result_inline?: string }>(taskID);
            const answer = parseAnswerFromDetail(detail);
            messages = messages.map((m) =>
              m.taskID === taskID && m.role === 'agent'
                ? { ...m, text: answer, pending: false }
                : m
            );
          } catch {
            messages = messages.map((m) =>
              m.taskID === taskID && m.role === 'agent'
                ? { ...m, text: '(could not read answer)', pending: false }
                : m
            );
          }
        } else if (eventType !== 'completed') {
          // Phase 106 (V1.2) AC-10 — task.failed or task.cancelled
          // populates an error bubble.
          const errorText = `Task ${eventType} — see Tasks page for details.`;
          messages = messages.map((m) =>
            m.taskID === taskID && m.role === 'agent'
              ? { ...m, text: errorText, role: 'system', pending: false }
              : m
          );
        }
        activeTaskID = null;
        void drainQueue();
      }
      es.onerror = () => {
        // EventSource auto-reconnects on transient drops; only nullify
        // on a permanent close to avoid resubscribe storms.
        if (es.readyState === EventSource.CLOSED) {
          taskEvents = null;
        }
      };
      taskEvents = es;
    } catch {
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

  async function cancelRun(): Promise<void> {
    if (chatClient === null) {
      return;
    }
    try {
      await chatClient.cancelRun(false);
      running = false;
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
    connection = resolveConnection();
    if (connection === null) {
      client = null;
      status = 'disconnected';
      return;
    }
    client = injectedClient ?? new HarborClient({ connection });
    canControl = hasScope(connection, 'admin');
    chatClient = buildChatClient(client);
    subscribeTaskLifecycle(client);

    void (async () => {
      try {
        const db = await openListPageDB(connection!);
        const operator = await operatorIdOf(
          connection!.identity.tenant,
          connection!.identity.user
        );
        savedFilters = new PlaygroundSavedFilters(db, operator);
        await refreshSavedViews();
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
  <PageHeader title="Playground" subtitle="Session chat · steering · overrides">
    {#snippet actions()}
      <PlaygroundHeader
        agents={agents}
        activeAgent={activeAgent}
        model={model}
        tokenCount={tokenCount}
        costUSD={costUSD}
        running={running}
        canImpersonate={canControl}
        impersonationTargets={impersonationTargets}
        activeImpersonation={activeImpersonation}
        onagentchange={(a) => (activeAgent = a)}
        oncancel={() => void cancelRun()}
        onrestart={() => void restartRun()}
        onimpersonate={(t) => (activeImpersonation = t)}
      />
    {/snippet}
  </PageHeader>

  <FilterBar>
    {#snippet saved()}
      <SavedViewChips
        views={savedViews}
        activeId={activeSavedId}
        onselect={applySavedView}
        ondelete={(id) => void deleteSavedView(id)}
      />
      <input
        class="control save-input"
        type="text"
        placeholder="Save current as…"
        bind:value={saveName}
        data-testid="playground-save-name"
        disabled={savedFilters === null}
        onkeydown={(e) => e.key === 'Enter' && void saveCurrentView()}
      />
      <button
        type="button"
        class="control"
        data-testid="playground-save-view"
        disabled={savedFilters === null || saveName.trim().length === 0}
        title={savedFilters === null
          ? 'Console-local saved-view store unavailable'
          : undefined}
        onclick={() => void saveCurrentView()}
      >
        Save view
      </button>
    {/snippet}
    {#snippet search()}
      <input
        class="control"
        type="search"
        placeholder="Filter messages…"
        data-testid="playground-message-search"
        disabled
        title="Message search — Post-V1"
      />
    {/snippet}
  </FilterBar>

  <div class="layout">
    <div class="main-col">
      {#if pageInfo !== null}
        <!--
          Phase 83w-F5 / D-164 — The chat surface is still functional
          on a planner/RunLoop runtime, so the page renders normally
          but surfaces a friendly banner explaining the topology absence
          above the chat. The pre-83w-F5 behaviour routed the whole
          page through PageState's red ERROR state with a Retry that
          would always fail.
        -->
        <p class="info-banner" data-testid="playground-topology-info">
          <strong>{pageInfo.headline}.</strong>
          {pageInfo.detail}
        </p>
      {/if}
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

      {#if status === 'ready'}
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

      <footer class="page-footer" data-testid="playground-footer">
        <span class="footer-item">
          {sending ? 'Streaming…' : 'Idle'}
        </span>
        <span class="footer-item">Protocol {PROTOCOL_VERSION}</span>
        <span class="footer-item">Console {CONSOLE_VERSION}</span>
      </footer>
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
  .page {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    padding: var(--space-6);
  }

  .layout {
    display: grid;
    grid-template-columns: 1fr var(--size-rail);
    gap: var(--space-4);
    align-items: start;
  }

  .main-col {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    min-width: var(--space-0);
  }

  .control {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
  }

  .control:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }

  .save-input {
    width: var(--size-search-min);
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

  .page-footer {
    display: flex;
    gap: var(--space-3);
    padding: var(--space-2);
    border-top: var(--border-hairline);
  }

  .footer-item {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  /* Phase 83w-F5 / D-164 — friendly info banner above the chat when
     topology.snapshot returned unknown_method. NOT a red error. */
  .info-banner {
    margin: var(--space-0);
    padding: var(--space-2) var(--space-3);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    background: var(--color-surface-raised);
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }
</style>
