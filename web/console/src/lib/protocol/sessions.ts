// Harbor Console — typed Sessions-page Protocol surface (D-121,
// CONVENTIONS.md §6; Phase 73c / D-122; D-424 projection).
//
// The unified `HarborClient` exposes a generic `sessions.*` namespace
// whose methods are typed `<R = unknown>`. This module is the thin
// typed view the Sessions page uses: it binds the generic namespace
// methods to the Sessions-page wire shapes in `$lib/sessions/types.ts`
// so the page is fully type-checked WITHOUT hand-rolling a `fetch`
// (CLAUDE.md §4.5 rule 5, §13) and WITHOUT a second top-level client.
// It also owns the chat-catalog request the Playground session switcher
// sends (`chatCatalogListRequest` — D-424).

import type { ProtocolClient } from './client.js';
import type {
  SessionsInspectResponse,
  SessionsListRequest,
  SessionsListResponse,
  SessionsSetTitleResponse
} from '../sessions/types.js';
import { DEFAULT_SESSION_LIST_LIMIT } from '../sessions/types.js';

/**
 * A typed wrapper over `client.sessions.*`. Each method binds the
 * generic namespace return to the Sessions-page wire shape. The
 * `identity` field is folded into the request body by the shared
 * transport — callers pass the request without `identity`.
 */
export class SessionsProtocol {
  readonly #client: ProtocolClient;

  constructor(client: ProtocolClient) {
    this.#client = client;
  }

  /** `sessions.list` — the paginated, filtered session catalog. */
  list(req: SessionsListRequest): Promise<SessionsListResponse> {
    return this.#client.sessions.list<SessionsListResponse>(
      req as unknown as Record<string, unknown>
    );
  }

  /** `sessions.inspect` — a single session's full snapshot. */
  inspect(sessionID: string): Promise<SessionsInspectResponse> {
    return this.#client.sessions.inspect<SessionsInspectResponse>(sessionID);
  }

  /**
   * `sessions.set_title` (D-288) — sets (non-empty `title`) or clears
   * (empty `title`) a session's human-readable name. The write scope is
   * the owning `(tenant, user)`, not own-session-only: `sessionID` may
   * name a sibling session of the caller's own `(tenant, user)`. Always
   * writes `manual` provenance; an over-bound / malformed title rejects
   * with a `ProtocolError` (400).
   */
  setTitle(sessionID: string, title: string): Promise<SessionsSetTitleResponse> {
    return this.#client.sessions.setTitle<SessionsSetTitleResponse>(sessionID, title);
  }
}

/**
 * The Playground chat-catalog's `sessions.list` request (D-424): the
 * session switcher needs ONLY the lifecycle catalog fields (id, title,
 * last activity) for its labels, so it EXPLICITLY requests the
 * `lifecycle` projection. It must NOT ask for a counter-dependent filter
 * or sort (`cost_above_cents` / `has_failed_task` / `has_intervention` /
 * `cost_desc`) — the runtime rejects those `invalid_request` on a
 * lifecycle request, because a lifecycle row has no counters to narrow or
 * order by (and a false-empty / mis-ordered page over them would
 * reproduce the false-absence class). No `sort` is sent, so the runtime's
 * default (`started_desc`) applies. Bounded to the default catalog page
 * size.
 */
export function chatCatalogListRequest(): SessionsListRequest {
  return {
    filter: {},
    limit: DEFAULT_SESSION_LIST_LIMIT,
    projection: 'lifecycle'
  };
}
