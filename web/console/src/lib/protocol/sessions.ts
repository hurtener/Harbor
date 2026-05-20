// Harbor Console — typed Sessions-page Protocol surface (D-121,
// CONVENTIONS.md §6; Phase 73c / D-122).
//
// The unified `HarborClient` exposes a generic `sessions.*` namespace
// whose methods are typed `<R = unknown>`. This module is the thin
// typed view the Sessions page uses: it binds the generic namespace
// methods to the Sessions-page wire shapes in `$lib/sessions/types.ts`
// so the page is fully type-checked WITHOUT hand-rolling a `fetch`
// (CLAUDE.md §4.5 rule 5, §13) and WITHOUT a second top-level client.

import type { ProtocolClient } from './client.js';
import type {
  SessionsInspectResponse,
  SessionsListRequest,
  SessionsListResponse
} from '../sessions/types.js';

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
}
