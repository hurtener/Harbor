// Harbor Console — the single Runtime-connection resolver (D-121,
// CONVENTIONS.md §6/§8).
//
// The Console attaches to a Harbor Runtime; the connection (base URL +
// bearer token + operator identity triple) is established by the Console
// shell and surfaced via the browser `localStorage` keys the `harbor console`
// boot writes. This module is the SINGLE resolver — a `.svelte` component
// never reads `localStorage` directly.
//
// The foundation audit found three legacy resolvers
// (`flows/connection.ts::resolveConnection`, the Tools-page
// `protocol/session.ts::resolveSession`, the Artifacts-page window-global
// shim). This module unifies them onto one storage convention. When no
// connection is configured (the Console is open but not yet attached, or
// running in a test harness), the resolver returns `null` — every page
// branches on `null` and renders `PageState`'s Disconnected state rather
// than issuing a request to nowhere. Disconnected is NOT an error.

/** The identity triple every Protocol request carries (CLAUDE.md §6). */
export interface ConnectionIdentity {
	tenant: string;
	user: string;
	session: string;
}

/** A resolved, live Runtime connection. */
export interface RuntimeConnection {
	/** The Runtime's Protocol base URL (e.g. `http://127.0.0.1:18080`). */
	baseURL: string;
	/** The bearer JWT carrying the verified identity + scope claims. */
	token: string;
	/** The operator's `(tenant, user, session)` isolation triple. */
	identity: ConnectionIdentity;
}

/**
 * The single storage convention. The `harbor console` boot writes these keys
 * once a Runtime connection is live; the Settings page (Phase pending) is the
 * operator-facing surface that mutates them.
 */
export const STORAGE_KEYS = {
	baseURL: 'harbor.runtime.base_url',
	token: 'harbor.runtime.token',
	tenant: 'harbor.runtime.tenant',
	user: 'harbor.runtime.user',
	session: 'harbor.runtime.session'
} as const;

/**
 * Resolve the active Runtime connection from browser storage.
 *
 * Returns `null` when the Console is not attached to a Runtime — any storage
 * key missing, or `localStorage` itself unavailable (SSR / test). Callers feed
 * the `null` into `<PageState>`, which renders the Disconnected state. A `null`
 * here is never an error — it is the honest "no Runtime" signal
 * (CONVENTIONS.md §4/§8).
 */
export function resolveConnection(): RuntimeConnection | null {
	if (typeof localStorage === 'undefined') {
		return null;
	}
	const baseURL = localStorage.getItem(STORAGE_KEYS.baseURL);
	const token = localStorage.getItem(STORAGE_KEYS.token);
	const tenant = localStorage.getItem(STORAGE_KEYS.tenant);
	const user = localStorage.getItem(STORAGE_KEYS.user);
	const session = localStorage.getItem(STORAGE_KEYS.session);
	if (!baseURL || !token || !tenant || !user || !session) {
		return null;
	}
	return {
		baseURL: baseURL.replace(/\/$/, ''),
		token,
		identity: { tenant, user, session }
	};
}

/** True when the Console is attached to a Runtime. */
export function isConnected(): boolean {
	return resolveConnection() !== null;
}
