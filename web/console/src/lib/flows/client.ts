// Harbor Console — typed Flows-page Protocol client (Phase 73i / D-117).
//
// This module is the ONLY place the Console issues an HTTP call against
// the six `POST /v1/flows/*` Runtime routes. `.svelte` components import
// `FlowsClient` and call its typed methods — a hand-rolled `fetch` in a
// component is a §13 forbidden practice (CLAUDE.md §4.5 #11).
//
// The client is a thin transport adapter: it serialises the typed
// request, attaches the bearer token + identity headers, decodes the
// typed response, and surfaces a typed `ProtocolError` on a non-2xx
// status. It NEVER silently degrades — a failed call rejects with a
// `FlowsClientError` carrying the canonical Protocol error code.
//
// When `cmd/harbor-gen-protocol-ts` (D-093) is extended to emit the
// flows.* method surface, this client is regenerated into
// `src/lib/protocol.ts`; until then it is the hand-authored mirror.

import type {
  FlowDescribeRequest,
  FlowDescription,
  FlowListRequest,
  FlowListResponse,
  FlowMetrics,
  FlowMetricsRequest,
  FlowRunDescribeRequest,
  FlowRunDescription,
  FlowRunRequest,
  FlowRunResponse,
  FlowRunsListRequest,
  FlowRunsListResponse,
  IdentityScope,
  ProtocolError,
} from './types';

/** Error thrown by the FlowsClient on a non-2xx Runtime response. */
export class FlowsClientError extends Error {
  /** The canonical Protocol error code (e.g. `identity_scope_required`). */
  readonly code: string;
  /** The HTTP status the Runtime returned. */
  readonly status: number;

  constructor(code: string, status: number, message: string) {
    super(message);
    this.name = 'FlowsClientError';
    this.code = code;
    this.status = status;
  }
}

/** Configuration the FlowsClient is constructed with. */
export interface FlowsClientConfig {
  /** Base URL of the Harbor Runtime (the `harbor console` proxy target). */
  baseURL: string;
  /** Bearer JWT carrying the verified identity + scope claims. */
  token: string;
  /** The operator's isolation triple, attached to every request. */
  identity: IdentityScope;
  /**
   * Injected fetch implementation. Defaults to the global `fetch`; tests
   * inject a stub so the client is exercisable without a live Runtime.
   */
  fetchImpl?: typeof fetch;
}

/**
 * The typed Flows-page Protocol client. One instance per Console
 * session; immutable after construction.
 */
export class FlowsClient {
  readonly #baseURL: string;
  readonly #token: string;
  readonly #identity: IdentityScope;
  readonly #fetch: typeof fetch;

  constructor(cfg: FlowsClientConfig) {
    this.#baseURL = cfg.baseURL.replace(/\/$/, '');
    this.#token = cfg.token;
    this.#identity = cfg.identity;
    this.#fetch = cfg.fetchImpl ?? fetch;
  }

  /** `flows.list` — paginated registered-flow catalog with aggregates. */
  async list(req: Omit<FlowListRequest, 'identity'>): Promise<FlowListResponse> {
    return this.#post<FlowListResponse>('/v1/flows/list', req);
  }

  /** `flows.describe` — a flow's full engine-graph description. */
  async describe(
    req: Omit<FlowDescribeRequest, 'identity'>,
  ): Promise<FlowDescription> {
    return this.#post<FlowDescription>('/v1/flows/describe', req);
  }

  /** `flows.runs.list` — a flow's paginated run history. */
  async runsList(
    req: Omit<FlowRunsListRequest, 'identity'>,
  ): Promise<FlowRunsListResponse> {
    return this.#post<FlowRunsListResponse>('/v1/flows/runs/list', req);
  }

  /** `flows.runs.describe` — a single run's per-node timeline. */
  async runsDescribe(
    req: Omit<FlowRunDescribeRequest, 'identity'>,
  ): Promise<FlowRunDescription> {
    return this.#post<FlowRunDescription>('/v1/flows/runs/describe', req);
  }

  /**
   * `flows.run` — invoke a one-shot run. The single mutating Flows-page
   * method; the Runtime gates it on the verified `admin` scope claim
   * (D-079) and rejects a claimless call with HTTP 403.
   */
  async run(req: Omit<FlowRunRequest, 'identity'>): Promise<FlowRunResponse> {
    return this.#post<FlowRunResponse>('/v1/flows/run', req);
  }

  /** `flows.metrics` — a flow's time-bucketed sparkline aggregates. */
  async metrics(
    req: Omit<FlowMetricsRequest, 'identity'>,
  ): Promise<FlowMetrics> {
    return this.#post<FlowMetrics>('/v1/flows/metrics', req);
  }

  /**
   * Issue a typed POST against a `/v1/flows/*` route. The operator
   * identity is folded into the body so the Runtime's defence-in-depth
   * identity check passes.
   */
  async #post<T>(route: string, body: object): Promise<T> {
    const res = await this.#fetch(`${this.#baseURL}${route}`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${this.#token}`,
        'X-Harbor-Tenant': this.#identity.tenant,
        'X-Harbor-User': this.#identity.user,
        'X-Harbor-Session': this.#identity.session,
      },
      body: JSON.stringify({ ...body, identity: this.#identity }),
    });
    const text = await res.text();
    if (!res.ok) {
      let code = 'runtime_error';
      let message = `flows request failed with HTTP ${res.status}`;
      if (text) {
        try {
          const err = JSON.parse(text) as ProtocolError;
          if (err.code) {
            code = err.code;
          }
          if (err.message) {
            message = err.message;
          }
        } catch {
          // Non-JSON body — keep the generic message; never swallow the
          // failure (CLAUDE.md §5 fail-loudly).
          message = `flows request failed (HTTP ${res.status}): ${text}`;
        }
      }
      throw new FlowsClientError(code, res.status, message);
    }
    return JSON.parse(text) as T;
  }
}
