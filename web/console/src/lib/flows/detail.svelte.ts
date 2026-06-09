// Harbor Console — Flow detail reactive state controller (Phase 108p / D-188).
// Svelte 5 runes mode (D-092).
//
// The detail-mode counterpart to `FlowsListState`: it owns the reactive state
// for one selected flow's detail route (`/flows/[flow_id]`). The `.svelte` view
// reads it and calls its actions, never touching the Protocol client directly
// (CONVENTIONS.md §6).
//
// It composes the shipped read surface (`flows.describe` — the engine graph +
// per-flow Budget; `flows.runs.list` — run history; `flows.runs.describe` — the
// per-run timeline) plus the one mutating action (`flows.run`, admin-gated —
// D-079), all through the one typed `FlowsProtocol` wrapper (no hand-rolled
// fetch — §13). The flow detail surface is a full-width read-only DAG canvas, so
// (unlike the other Phase 108 pages) the detail stays its own route rather than
// collapsing into a rail (D-188).

import {
  resolveConnection,
  hasScope,
  type RuntimeConnection
} from '$lib/connection.js';
import { HarborClient } from '$lib/protocol/harbor.js';
import { FlowsProtocol } from '$lib/protocol/flows.js';
import type { PageStatus } from '$lib/components/ui/PageState.svelte';
import type { StatusKind } from '$lib/components/ui/StatusChip.svelte';
import { toPageError, health, type PageError } from '$lib/flows/derive.js';
import type {
  FlowDescription,
  FlowNode,
  FlowRun,
  FlowRunDescription
} from '$lib/flows/types.js';
import type { GraphInput } from '$lib/components/graph/types.js';

export class FlowDetailState {
  /* ---- connection + client (CONVENTIONS.md §6) ------------------- */
  connection = $state<RuntimeConnection | null>(null);
  canRun = $state(false);
  disconnected = $derived(this.connection === null);

  /* ---- the flow under view --------------------------------------- */
  flowID = $state('');

  /* ---- detail async state (four-state contract) ------------------ */
  status = $state<PageStatus>('loading');
  loadError = $state<PageError | null>(null);
  description = $state<FlowDescription | null>(null);
  runs = $state<FlowRun[]>([]);

  /* ---- run-summary rail (nested PageState) ----------------------- */
  selectedRunId = $state<string | null>(null);
  runSummary = $state<FlowRunDescription | null>(null);
  runSummaryStatus = $state<PageStatus>('empty');
  runSummaryError = $state<PageError | null>(null);
  selectedNodeId = $state<string | null>(null);

  /* ---- run-flow modal (flows.run — D-079) ------------------------ */
  runOpen = $state(false);
  runPending = $state(false);
  runError = $state<string | null>(null);

  /* ---- compare-versions (Console-local diff, D-061) -------------- */
  compareOpen = $state(false);
  snapshot = $state<FlowDescription | null>(null);

  #flows: FlowsProtocol | null = null;

  /** The shared graph-canvas input, projected from the flow description. */
  graphInput = $derived.by<GraphInput>(() => {
    const d = this.description;
    if (!d) return { nodes: [], edges: [] };
    return {
      nodes: d.nodes.map((n) => ({
        id: n.id,
        label: n.descriptor || n.id,
        kind: n.type,
        meta: n.policy
          ? {
              retries: String(n.policy.max_retries ?? 0),
              timeout_ms: String(n.policy.timeout_ms ?? 0)
            }
          : undefined
      })),
      edges: d.edges.map((e) => ({ from: e.from, to: e.to }))
    };
  });

  /** The detail header health pill. */
  health = $derived.by<{ label: string; kind: StatusKind }>(() =>
    health(this.description)
  );

  /* ================================================================ */
  /* Boot + load (re-runnable on flow-id change)                       */
  /* ================================================================ */

  load(flowID: string, injectedFlows?: FlowsProtocol): void {
    this.flowID = flowID;
    const connection = resolveConnection();
    this.connection = connection;
    this.canRun = hasScope(connection, 'admin');
    if (injectedFlows !== undefined) {
      this.#flows = injectedFlows;
    } else if (connection !== null) {
      this.#flows = new FlowsProtocol(new HarborClient({ connection }));
    } else {
      this.#flows = null;
    }
    // Reset the run-summary rail whenever the flow changes.
    this.selectedRunId = null;
    this.runSummary = null;
    this.runSummaryStatus = 'empty';
    this.selectedNodeId = null;
    void this.loadDetail();
  }

  async loadDetail(): Promise<void> {
    if (this.#flows === null || this.flowID === '') {
      this.status = 'disconnected';
      return;
    }
    this.status = 'loading';
    this.loadError = null;
    try {
      const [desc, runsResp] = await Promise.all([
        this.#flows.describe(this.flowID),
        this.#flows.runsList({ flow_id: this.flowID })
      ]);
      this.description = desc;
      this.runs = runsResp.runs ?? [];
      this.status = 'ready';
    } catch (e) {
      this.description = null;
      this.runs = [];
      this.loadError = toPageError(e);
      this.status = 'error';
    }
  }

  refresh(): void {
    void this.loadDetail();
  }

  /* ================================================================ */
  /* Run summary (flows.runs.describe)                                 */
  /* ================================================================ */

  async selectRun(runID: string): Promise<void> {
    this.selectedRunId = runID;
    if (this.#flows === null) {
      this.runSummaryStatus = 'disconnected';
      return;
    }
    this.runSummaryStatus = 'loading';
    this.runSummaryError = null;
    try {
      this.runSummary = await this.#flows.runsDescribe(runID);
      this.runSummaryStatus = 'ready';
    } catch (e) {
      this.runSummary = null;
      this.runSummaryError = toPageError(e);
      this.runSummaryStatus = 'error';
    }
  }

  selectNode(nodeID: string | null): void {
    this.selectedNodeId = nodeID;
  }

  /** Returns the tool descriptor for a node when it is a tool call, else
   * `null` — the page uses this to decide a tools-page navigation. */
  toolDescriptorFor(nodeID: string): string | null {
    const node: FlowNode | undefined = this.description?.nodes.find(
      (n) => n.id === nodeID
    );
    if (node?.type === 'tool' && node.descriptor) return node.descriptor;
    return null;
  }

  /* ================================================================ */
  /* Compare-versions (Console-local, D-061)                           */
  /* ================================================================ */

  saveSnapshot(): void {
    this.snapshot = this.description;
  }

  openCompare(): void {
    this.compareOpen = true;
  }

  closeCompare(): void {
    this.compareOpen = false;
  }

  /* ================================================================ */
  /* Run flow (flows.run — §13 / D-079)                                */
  /* ================================================================ */

  openRun(): void {
    this.runOpen = true;
    this.runError = null;
  }

  cancelRun(): void {
    this.runOpen = false;
    this.runError = null;
  }

  /** Submits a real `flows.run`, then reloads the detail (so the new run
   * appears in the history). No-op without the admin claim. */
  async submitRun(inputs: Record<string, unknown>): Promise<void> {
    if (this.#flows === null) return;
    this.runPending = true;
    this.runError = null;
    try {
      await this.#flows.run({ flow_id: this.flowID, inputs });
      this.runOpen = false;
      await this.loadDetail();
    } catch (e) {
      const err = toPageError(e);
      this.runError = `${err.code}: ${err.message}`;
    } finally {
      this.runPending = false;
    }
  }
}
