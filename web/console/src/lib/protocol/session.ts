/**
 * Console Protocol session helper (Phase 73f / D-116).
 *
 * Resolves the data a typed Protocol client needs to reach the Runtime:
 * the base URL, the bearer token, and the active identity triple. This
 * is a minimal V1 shim — the full session / runtime-registry surface
 * lands in other Console phases (Settings / runtime address book). The
 * Tools page uses this to construct a `ToolsClient` without re-deriving
 * the wiring.
 *
 * D-091: the Console is served by `harbor console`, which proxies the
 * Protocol surface on its own origin — so the Runtime base URL is the
 * page's own origin. The auth token is the WebCrypto-decrypted JWT the
 * Console persists; the Playwright harness seeds the raw token under
 * the well-known `harbor.console.token` storage key.
 */

import type { ToolsIdentityScope } from './tools.js';

/** The localStorage key the Console reads its session token from (D-091). */
export const AUTH_STORAGE_KEY = 'harbor.console.token';

/** The resolved inputs a typed Protocol client needs. */
export interface ProtocolSession {
  baseURL: string;
  token: string;
  identity: ToolsIdentityScope;
}

/**
 * Decodes the `(tenant, user, session)` triple from a JWT's claims
 * without verifying the signature — verification is the Runtime's job;
 * the Console only needs the identity to populate request bodies. A
 * malformed token yields an all-empty triple, which the Runtime then
 * rejects loudly with `identity_required` (fail-loud, never silent).
 */
function identityFromToken(token: string): ToolsIdentityScope {
  const empty: ToolsIdentityScope = { tenant: '', user: '', session: '' };
  const parts = token.split('.');
  if (parts.length !== 3) {
    return empty;
  }
  try {
    const payload = parts[1].replace(/-/g, '+').replace(/_/g, '/');
    const claims = JSON.parse(atob(payload)) as Record<string, unknown>;
    return {
      tenant: typeof claims.tenant === 'string' ? claims.tenant : '',
      user: typeof claims.user === 'string' ? claims.user : '',
      session: typeof claims.session === 'string' ? claims.session : ''
    };
  } catch {
    return empty;
  }
}

/**
 * Resolves the active Protocol session from browser storage. Returns
 * `null` when no token is present — the caller renders the
 * unauthenticated state rather than throwing.
 */
export function resolveSession(): ProtocolSession | null {
  if (typeof window === 'undefined') {
    return null;
  }
  const token = window.localStorage.getItem(AUTH_STORAGE_KEY);
  if (token === null || token.length === 0) {
    return null;
  }
  return {
    baseURL: window.location.origin,
    token,
    identity: identityFromToken(token)
  };
}
