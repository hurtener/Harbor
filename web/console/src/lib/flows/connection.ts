// Harbor Console — Flows-page Runtime-connection resolver (Phase 73i /
// D-117).
//
// The Console attaches to a Harbor Runtime; the connection (base URL +
// bearer token + operator identity) is established by the Console shell
// and, at V1, surfaced to the page via the browser `localStorage` keys
// the `harbor console` boot writes. This module is the single resolver
// — a `.svelte` component never reads `localStorage` directly.
//
// When no connection is configured (the Console is open but not yet
// attached to a Runtime, or running in a test harness pre-73m), the
// resolver returns `null`; the page renders its "not connected" state
// rather than crashing or issuing a request to nowhere.

import { FlowsClient } from './client';
import type { IdentityScope } from './types';

/** The keys the Console shell writes once a Runtime connection is live. */
const STORAGE_BASE_URL = 'harbor.runtime.base_url';
const STORAGE_TOKEN = 'harbor.runtime.token';
const STORAGE_TENANT = 'harbor.runtime.tenant';
const STORAGE_USER = 'harbor.runtime.user';
const STORAGE_SESSION = 'harbor.runtime.session';

/** A resolved Runtime connection. */
export interface RuntimeConnection {
  baseURL: string;
  token: string;
  identity: IdentityScope;
}

/**
 * Resolve the active Runtime connection from browser storage. Returns
 * null when the Console is not attached to a Runtime — every component
 * branches on null rather than assuming a live connection.
 */
export function resolveConnection(): RuntimeConnection | null {
  if (typeof localStorage === 'undefined') {
    return null;
  }
  const baseURL = localStorage.getItem(STORAGE_BASE_URL);
  const token = localStorage.getItem(STORAGE_TOKEN);
  const tenant = localStorage.getItem(STORAGE_TENANT);
  const user = localStorage.getItem(STORAGE_USER);
  const session = localStorage.getItem(STORAGE_SESSION);
  if (!baseURL || !token || !tenant || !user || !session) {
    return null;
  }
  return { baseURL, token, identity: { tenant, user, session } };
}

/**
 * Build a `FlowsClient` for the active connection, or null when no
 * connection is configured.
 */
export function flowsClientFromConnection(): FlowsClient | null {
  const conn = resolveConnection();
  if (!conn) {
    return null;
  }
  return new FlowsClient({
    baseURL: conn.baseURL,
    token: conn.token,
    identity: conn.identity,
  });
}

/**
 * True when the resolved connection carries the `admin` scope claim —
 * the entitlement `flows.run` requires (D-079). The Console shell
 * persists the verified scope set; V1 reads the `admin` flag from a
 * dedicated storage key.
 */
export function hasRunScope(): boolean {
  if (typeof localStorage === 'undefined') {
    return false;
  }
  const scopes = localStorage.getItem('harbor.runtime.scopes') ?? '';
  return scopes
    .split(',')
    .map((s) => s.trim())
    .includes('admin');
}
