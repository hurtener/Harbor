// Harbor Console — typed Flows-page Protocol surface (D-121,
// CONVENTIONS.md §6).
//
// The unified `HarborClient` exposes a generic `flows.*` namespace whose
// methods are typed `<R = unknown>`. This module is the thin typed view
// the Flows page uses: it binds the generic namespace methods to the
// Flows-page wire shapes in `$lib/flows/types.ts` so the page is fully
// type-checked WITHOUT hand-rolling a `fetch` (CLAUDE.md §4.5 rule 5,
// §13) and WITHOUT a second top-level client.
//
// The audit's fifth legacy client — the page-local `FlowsClient` in
// `$lib/flows/client.ts` — is deleted; this module replaces it. Every
// `flows.*` call routes through the one `HarborClient` transport choke
// point. The wire types stay in `$lib/flows/types.ts` (the hand-authored
// mirror of `internal/protocol/types/flows.go`) until the D-093
// generator emits them.

import type { ProtocolClient } from './client.js';
import type {
  FlowDescription,
  FlowListRequest,
  FlowListResponse,
  FlowMetrics,
  FlowMetricsRequest,
  FlowRunDescription,
  FlowRunRequest,
  FlowRunResponse,
  FlowRunsListRequest,
  FlowRunsListResponse
} from '../flows/types.js';

/**
 * A typed wrapper over `client.flows.*`. Each method binds the generic
 * namespace return to the Flows-page wire shape. The `identity` field is
 * folded into the request body by the shared transport — callers pass
 * the request without `identity` (the `Omit` below).
 */
export class FlowsProtocol {
  readonly #client: ProtocolClient;

  constructor(client: ProtocolClient) {
    this.#client = client;
  }

  /** `flows.list` — the paginated registered-flow catalog. */
  list(req: Omit<FlowListRequest, 'identity'>): Promise<FlowListResponse> {
    return this.#client.flows.list<FlowListResponse>(req);
  }

  /** `flows.describe` — a flow's full engine-graph description. */
  describe(id: string): Promise<FlowDescription> {
    return this.#client.flows.describe<FlowDescription>({ id });
  }

  /** `flows.runs.list` — a flow's paginated run history. */
  runsList(
    req: Omit<FlowRunsListRequest, 'identity'>
  ): Promise<FlowRunsListResponse> {
    return this.#client.flows.runsList<FlowRunsListResponse>(req);
  }

  /** `flows.runs.describe` — a single run's per-node timeline. */
  runsDescribe(runID: string): Promise<FlowRunDescription> {
    return this.#client.flows.runsDescribe<FlowRunDescription>({ run_id: runID });
  }

  /** `flows.run` — the single mutating Flows-page method (D-079 scope-gated). */
  run(req: Omit<FlowRunRequest, 'identity'>): Promise<FlowRunResponse> {
    return this.#client.flows.run<FlowRunResponse>(req);
  }

  /** `flows.metrics` — a flow's time-bucketed sparkline aggregates. */
  metrics(req: Omit<FlowMetricsRequest, 'identity'>): Promise<FlowMetrics> {
    return this.#client.flows.metrics<FlowMetrics>(req);
  }
}
