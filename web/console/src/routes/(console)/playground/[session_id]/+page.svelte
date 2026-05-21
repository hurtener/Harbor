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
  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import PageHeader from '$lib/components/ui/PageHeader.svelte';
  import FilterBar from '$lib/components/ui/FilterBar.svelte';
  import SavedViewChips, { type SavedView } from '$lib/components/ui/SavedViewChips.svelte';
  import DetailRail from '$lib/components/ui/DetailRail.svelte';
  import RailCard from '$lib/components/ui/RailCard.svelte';
  import Pagination from '$lib/components/ui/Pagination.svelte';
  import ConnectionFooter from '$lib/components/ui/ConnectionFooter.svelte';
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
  import { ProtocolError } from '$lib/protocol/errors.js';
  import type { TopologyProjection } from '$lib/protocol/topology.js';
  import { resolveConnection, hasScope, type RuntimeConnection } from '$lib/connection.js';
  import { openListPageDB } from '$lib/db/console_db.js';
  import { operatorIdOf } from '$lib/db/schema.js';
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
  function buildChatClient(c: ProtocolClient): ChatProtocolClient {
    return {
      async sendMessage(text, artifactIDs) {
        // The SHIPPED Phase 54 `user_message` control verb — NO parallel
        // chat protocol. The override (set via `runs.set_overrides`) is
        // consumed by the runtime on this message.
        await c.control.dispatch('user_message', sessionID, {
          text,
          artifact_ids: artifactIDs
        });
        return { taskID: sessionID };
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
        // `artifacts.put` — the Console upload pipeline. Heavy bytes go
        // to the artifact store; the chat carries only the reference.
        const resp = await c.artifacts.put<{ id: string }>({
          filename: file.name,
          mime: file.type || 'application/octet-stream',
          size_bytes: file.size
        });
        return {
          id: resp.id,
          mime: file.type || 'application/octet-stream',
          filename: file.name,
          sizeBytes: file.size
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
        await c.control.dispatch('start', '', {
          query: '',
          description: `Playground restart · ${activeAgent}`
        });
        return { taskID: sessionID };
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
  /* Loading                                                           */
  /* ================================================================ */

  async function load(): Promise<void> {
    if (client === null) {
      status = 'disconnected';
      return;
    }
    status = 'loading';
    pageError = null;
    try {
      // The Playground opens against a live session — V1 starts with an
      // empty stream and grows as the operator sends messages. The
      // initial load proves the connection + Protocol surface are live
      // by fetching the topology snapshot (also feeds the trace toggle).
      await client.topology.snapshot<TopologyProjection>();
      status = messages.length === 0 ? 'empty' : 'ready';
    } catch (err) {
      pageError = toError(err);
      status = 'error';
    }
  }

  /* ================================================================ */
  /* Chat actions                                                      */
  /* ================================================================ */

  async function sendMessage(text: string, artifactIDs: string[]): Promise<void> {
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
      await chatClient.sendMessage(text, artifactIDs);
      // The runtime answers asynchronously over the event stream; V1
      // surfaces an acknowledgement bubble. A live token stream is the
      // event-subscription wiring shared with the Live Runtime page.
      messages = [
        ...messages,
        {
          id: `m-${Date.now()}-a`,
          role: 'agent',
          text: 'Message accepted by the Runtime.',
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
      status = 'empty';
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
      const proj = await client.topology.snapshot<TopologyProjection>();
      traceNodes = proj.nodes.map((n) => ({ id: n.name, kind: n.kind }));
    } catch (err) {
      traceError = toError(err).message;
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
        placeholder="Save preset as…"
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
        Save
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
      <PageState status={status} error={pageError} onretry={() => void load()}>
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
            onsend={(text, ids) => void sendMessage(text, ids)}
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

  <ConnectionFooter />
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
</style>
