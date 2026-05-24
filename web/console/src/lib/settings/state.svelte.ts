// Console Settings page reactive state (Phase 73m / D-129 — Svelte 5
// runes mode, D-092).
//
// This module owns the Settings page's reactive state; the `.svelte`
// components read it and call its actions, never touching the Protocol
// client directly. The Settings page is a pure CONSUMER of the 72f /
// 72g posture surfaces — it composes `runtime.info` + `runtime.drivers`
// + `governance.posture` + `llm.posture` reads. The ONE net-new method
// it owns is `auth.rotate_token` (admin).
//
// CONVENTIONS.md §4/§8: `connection.ts` returning `null` is the
// Disconnected state, DISTINCT from Error. Every Protocol call routes
// through the unified `HarborClient` (the single `fetch` choke point)
// and rejects with the one `ProtocolError` carrying `(code, message,
// status)`.

import { resolveConnection } from '$lib/connection.js';
import { HarborClient } from '$lib/protocol/harbor.js';
import { ProtocolError } from '$lib/protocol/errors.js';
import type { PageStatus } from '$lib/components/ui/PageState.svelte';
import type {
	RuntimeInfo,
	RuntimeDrivers,
	GovernancePostureResponse,
	LLMPostureResponse,
	AuthRotateTokenResponse
} from '$lib/protocol/settings.js';

/** A page-friendly error projection. */
export interface PageError {
	code: string;
	message: string;
}

/**
 * Builds a `HarborClient` from the resolved Runtime connection, or
 * returns `null` when the Console is not attached. A `null` is the
 * honest "no Runtime" signal — the caller renders `PageState`'s
 * Disconnected state, never an Error (CONVENTIONS.md §8).
 */
function buildClient(): HarborClient | null {
	const connection = resolveConnection();
	if (connection === null) {
		return null;
	}
	return new HarborClient({ connection });
}

/**
 * describeError renders a `ProtocolError` into a page-friendly message,
 * keeping the canonical code visible so the operator knows the recovery.
 */
export function describeError(e: unknown): PageError {
	if (e instanceof ProtocolError) {
		switch (e.code) {
			case 'identity_scope_required':
			case 'scope_mismatch':
				return { code: e.code, message: 'This action requires the admin scope claim.' };
			case 'identity_required':
				return {
					code: e.code,
					message: 'Identity scope is incomplete — re-attach to the runtime.'
				};
			default:
				return { code: e.code, message: e.message };
		}
	}
	if (e instanceof Error) {
		return { code: 'runtime_error', message: e.message };
	}
	return { code: 'runtime_error', message: 'Unknown error' };
}

/**
 * The Settings sections, in section-nav-rail order. Each is one anchor
 * the sub-nav rail scrolls to.
 *
 * The `group` field discriminates by DATA DEPENDENCY (Phase 83p / D-158):
 *   - `console-local` — reads from the Console-side DB
 *     (`db.runtimes` / `db.authProfiles` / `db.pats` / `db.profile` /
 *     `db.keybindings` / `db.routing`). Renders unconditionally; works
 *     when no Runtime is attached.
 *   - `runtime-posture` — reads from `settings.posture.*` (the four
 *     read-only posture methods on the Runtime). Wraps in `<PageState>`
 *     so the disconnected state shows the standard placeholder instead
 *     of an empty card per section.
 *
 * Before 83p, the page template wrapped EVERY section in `<PageState>`,
 * which hid the Connected Runtimes form behind the disconnected
 * placeholder — defeating the operator's only path to attach a Runtime.
 * `SettingsState.load()`'s docstring documented the intended split but
 * the template ignored it. 83p moves the split into structure.
 */
export const SETTINGS_SECTIONS = [
	{ id: 'connected-runtimes', label: 'Connected Runtimes', group: 'console-local' },
	{ id: 'per-runtime-auth', label: 'Per-Runtime Auth', group: 'console-local' },
	{ id: 'api-tokens', label: 'API Tokens', group: 'console-local' },
	{ id: 'appearance', label: 'Appearance', group: 'console-local' },
	{ id: 'time-locale', label: 'Time & Locale', group: 'console-local' },
	{ id: 'keybindings', label: 'Keybindings', group: 'console-local' },
	{ id: 'notifications-routing', label: 'Notifications Routing', group: 'console-local' },
	{ id: 'runtime-info', label: 'Runtime Info', group: 'runtime-posture' },
	{ id: 'governance-posture', label: 'Governance Posture', group: 'runtime-posture' },
	{ id: 'storage-drivers', label: 'Storage Drivers', group: 'runtime-posture' },
	{ id: 'llm-posture', label: 'LLM-Provider Posture', group: 'runtime-posture' },
	{ id: 'about', label: 'About', group: 'runtime-posture' }
] as const;

export type SettingsSectionId = (typeof SETTINGS_SECTIONS)[number]['id'];
export type SettingsSectionGroup = (typeof SETTINGS_SECTIONS)[number]['group'];

/**
 * consoleLocalSections returns the subset of SETTINGS_SECTIONS that
 * does NOT depend on a Runtime connection. Used by the page template
 * to render these sections OUTSIDE the `<PageState>` boundary so they
 * always work (Phase 83p / D-158).
 */
export function consoleLocalSections(): readonly (typeof SETTINGS_SECTIONS)[number][] {
	return SETTINGS_SECTIONS.filter((s) => s.group === 'console-local');
}

/**
 * runtimePostureSections returns the subset of SETTINGS_SECTIONS that
 * reads from the Runtime's posture surface. Used by the page template
 * to render these sections INSIDE `<PageState>` so the disconnected /
 * error states show one consolidated placeholder + Retry button.
 */
export function runtimePostureSections(): readonly (typeof SETTINGS_SECTIONS)[number][] {
	return SETTINGS_SECTIONS.filter((s) => s.group === 'runtime-posture');
}

/**
 * The runtime-posture bundle the Settings page renders — composed from
 * the 72f / 72g posture methods. All four are read in one `load()`
 * fan-out; a partial failure surfaces in `PageState`'s Error state.
 */
export interface RuntimePosture {
	info: RuntimeInfo | null;
	drivers: RuntimeDrivers | null;
	governance: GovernancePostureResponse | null;
	llm: LLMPostureResponse | null;
}

/**
 * SettingsState owns the Settings page's reactive state. It exposes a
 * `PageStatus` (CONVENTIONS.md §4 four-state contract) the `<PageState>`
 * boundary consumes, plus the composed runtime posture.
 */
export class SettingsState {
	/** The four-state async status the `<PageState>` boundary reads. */
	status = $state<PageStatus>('loading');
	/** The thrown error — populated only in the `error` status. */
	error = $state<PageError | null>(null);
	/** The composed runtime posture (suppressed while in `error`). */
	posture = $state<RuntimePosture>({
		info: null,
		drivers: null,
		governance: null,
		llm: null
	});
	/** The active section anchor (drives the sub-nav rail highlight). */
	activeSection = $state<SettingsSectionId>('connected-runtimes');

	/** True when the bound runtime is running in dev-mock LLM mode. */
	get mockMode(): boolean {
		return this.posture.llm?.mock_mode === true;
	}

	/**
	 * load fans out the four read-only posture methods. When the Console
	 * is not attached to a Runtime it goes to the Disconnected state —
	 * never Error (CONVENTIONS.md §8).
	 *
	 * The Settings page is section-based: the Console-local sections
	 * (Connected Runtimes, Per-Runtime Auth, Appearance, …) do NOT
	 * depend on the runtime posture, only the four read-only posture
	 * cards do. A posture method that fails therefore must NOT blank the
	 * whole page — the page reaches `ready` and each posture card renders
	 * its own "unavailable" state from a `null`. The page is `error`
	 * ONLY when the whole posture fan-out fails (e.g. the runtime
	 * rejected every read — a real connection-level failure), so the
	 * operator still gets a Retry. A partial failure degrades per-card,
	 * never silently (each card shows the missing data explicitly).
	 */
	async load(): Promise<void> {
		const client = buildClient();
		if (client === null) {
			this.status = 'disconnected';
			return;
		}
		this.status = 'loading';
		this.error = null;
		const [info, drivers, governance, llm] = await Promise.all([
			client.posture.info<RuntimeInfo>().catch((e) => {
				this.error = describeError(e);
				return null;
			}),
			client.posture.drivers<RuntimeDrivers>().catch(() => null),
			client.posture.governance<GovernancePostureResponse>().catch(() => null),
			client.posture.llm<LLMPostureResponse>().catch(() => null)
		]);
		this.posture = { info, drivers, governance, llm };
		// Hard error only when EVERY posture read failed — a real
		// connection-level failure the operator must retry. A partial
		// failure degrades per-card (the local sections still work).
		if (info === null && drivers === null && governance === null && llm === null) {
			this.status = 'error';
			if (this.error === null) {
				this.error = { code: 'runtime_error', message: 'Runtime posture unavailable.' };
			}
			return;
		}
		this.error = null;
		this.status = 'ready';
	}
}

/**
 * RotateTokenState owns the Settings page's `auth.rotate_token` action
 * — the ONE net-new Protocol method this phase ships. The action is
 * ADMIN-gated; a connection without the `admin` scope claim renders the
 * control disabled-with-tooltip (CONVENTIONS.md §5 — no stubbed action
 * presented as done). The re-minted token is ONE-TIME-REVEAL: it is
 * held in `revealedToken` until the operator dismisses the reveal, then
 * dropped.
 */
export class RotateTokenState {
	/** 'idle' | 'rotating' | 'revealed' | 'error'. */
	phase = $state<'idle' | 'rotating' | 'revealed' | 'error'>('idle');
	/** The one-time-revealed re-minted token; cleared on dismiss. */
	revealedToken = $state<string | null>(null);
	/** The new token's expiry, ISO-8601; cleared on dismiss. */
	expiresAt = $state<string | null>(null);
	/** The error from a failed rotation. */
	error = $state<PageError | null>(null);

	/** True when the resolved connection carries the `admin` scope claim. */
	get hasAdminScope(): boolean {
		const conn = resolveConnection();
		return conn !== null && conn.scopes.includes('admin');
	}

	/** True when the Console is attached to a Runtime. */
	get connected(): boolean {
		return resolveConnection() !== null;
	}

	/**
	 * rotate invokes the real `auth.rotate_token` Protocol method. The
	 * control is rendered disabled when `hasAdminScope` is false, so a
	 * non-admin operator never reaches this — but the runtime ALSO gates
	 * (D-079), so a forged call fails closed with a 403 the Error phase
	 * surfaces.
	 */
	async rotate(): Promise<void> {
		const client = buildClient();
		if (client === null) {
			this.phase = 'error';
			this.error = { code: 'disconnected', message: 'Not attached to a Runtime.' };
			return;
		}
		this.phase = 'rotating';
		this.error = null;
		try {
			const resp = await client.auth.rotateToken<AuthRotateTokenResponse>();
			this.revealedToken = resp.new_token;
			this.expiresAt = resp.expires_at;
			this.phase = 'revealed';
		} catch (e) {
			this.error = describeError(e);
			this.phase = 'error';
		}
	}

	/**
	 * dismiss drops the one-time-revealed token from memory. After
	 * dismiss the Console can NEVER display the raw token again — the
	 * operator must have copied it (the one-time-reveal contract).
	 */
	dismiss(): void {
		this.revealedToken = null;
		this.expiresAt = null;
		this.phase = 'idle';
		this.error = null;
	}
}
